// Package store owns the sqlite database: opening it with the right pragmas
// and bringing its schema up to date with the embedded migrations.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrSchemaTooNew is returned by Open when the database was migrated by a
// newer hookwitness than this one.
var ErrSchemaTooNew = errors.New("database schema is newer than this binary")

// Store is an open, migrated database.
type Store struct {
	db *sql.DB
}

type migration struct {
	version int
	name    string
	sql     string
}

// Open opens (creating if missing) the sqlite file at dbPath and applies any
// pending migrations. A database whose schema is newer than the newest
// embedded migration is refused with ErrSchemaTooNew.
func Open(ctx context.Context, dbPath string) (*Store, error) {
	return open(ctx, dbPath, migrationFS)
}

func open(ctx context.Context, dbPath string, migrations fs.FS) (*Store, error) {
	ms, err := loadMigrations(migrations)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	if err := migrate(ctx, db, ms); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// dsn builds the modernc.org/sqlite connection string. The pragmas are set on
// every new connection of the pool, not once.
func dsn(dbPath string) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_txlock", "immediate")
	return "file:" + dbPath + "?" + q.Encode()
}

func loadMigrations(fsys fs.FS) ([]migration, error) {
	names, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	var ms []migration
	seen := map[int]string{}
	for _, name := range names {
		base := path.Base(name)
		num, _, ok := strings.Cut(base, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %s: name must start with a positive number and _", base)
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %s and %s share version %d", prev, base, v)
		}
		seen[v] = base
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: v, name: base, sql: string(body)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	return ms, nil
}

func migrate(ctx context.Context, db *sql.DB, ms []migration) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	var current int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	newest := 0
	if len(ms) > 0 {
		newest = ms[len(ms)-1].version
	}
	if current > newest {
		return fmt.Errorf("%w: database is at version %d, this binary knows up to %d; upgrade hookwitness or use another database file",
			ErrSchemaTooNew, current, newest)
	}
	for _, m := range ms {
		if m.version <= current {
			continue
		}
		if err := apply(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func apply(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		m.version, m.name, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("migration %s: record: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %s: commit: %w", m.name, err)
	}
	return nil
}

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// SchemaVersion returns the newest applied migration.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }
