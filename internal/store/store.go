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
	"math/rand/v2"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modernc.org/sqlite" // also registers the "sqlite" driver
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrSchemaTooNew is returned by Open when the database was migrated by a
// newer hookwitness than this one.
var ErrSchemaTooNew = errors.New("database schema is newer than this binary")

// ErrInUse is returned by Open while the database is being compacted, and
// by Compact while another process has it open (a running server).
var ErrInUse = errors.New("the database is in use by another process")

var errLocked = errors.New("locked")

// Store is an open, migrated database.
type Store struct {
	db   *sql.DB
	path string   // the database file
	lock *os.File // a shared lock on <db>.lock while open (Compact takes it exclusively)
	// nudge wakes the housekeeping for a size check after a large body
	// (evict.go); full means one is already pending.
	nudge chan struct{}
	// stuck: the last eviction could not get under its target, because
	// what is left is pinned (Storage warns of it).
	stuck atomic.Bool
	// evictStarted, in tests, runs once an eviction has read the newest
	// request it works on.
	evictStarted func()
	// wmu serialises writes inside this process: SQLite has one writer at a
	// time anyway, and queueing here is cheaper than retrying SQLITE_BUSY.
	wmu sync.Mutex
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
	lock, err := lockFile(dbPath+".lock", false)
	if errors.Is(err, errLocked) {
		return nil, fmt.Errorf("open %s: %w: it is being compacted (hookwitness db compact); try again when that ends", dbPath, ErrInUse)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: lock: %w", dbPath, err)
	}
	// A new database is private, like the key file: it holds every body,
	// hashed token and sealed secret (SQLite gives its -wal and -shm the
	// file's mode). An existing file keeps the mode it has.
	if err := createPrivate(dbPath); err != nil && !errors.Is(err, fs.ErrExist) {
		lock.Close()
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		lock.Close()
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	// Two processes starting on the same new file race for the switch to WAL,
	// and SQLite reports that as SQLITE_BUSY without waiting on busy_timeout.
	// The loser retries the whole setup for a while; migrations are idempotent.
	deadline := time.Now().Add(setupRetry)
	for {
		err = setup(ctx, db, dbPath, ms)
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(time.Duration(10+rand.IntN(40)) * time.Millisecond):
			continue
		}
		break
	}
	if err != nil {
		db.Close()
		lock.Close()
		return nil, err
	}
	return &Store{db: db, path: dbPath, lock: lock, nudge: make(chan struct{}, 1)}, nil
}

// setupRetry bounds how long Open keeps retrying a busy database.
const setupRetry = 10 * time.Second

func setup(ctx context.Context, db *sql.DB, dbPath string, ms []migration) error {
	if err := initFile(ctx, dbPath); err != nil {
		return fmt.Errorf("open %s: %w", dbPath, err)
	}
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("open %s: %w", dbPath, err)
	}
	return migrate(ctx, db, ms)
}

// initFile gives a new, empty database file incremental auto-vacuum (space
// can go back to the file system, Reclaim) and WAL, on a connection of its
// own, before the pool's first connection writes the file's first page:
// auto-vacuum can only be chosen before that page exists. An existing
// database is only read (its page count) and keeps its mode (Compact
// converts it). It is not one of the pool's pragmas because setting it is
// a write: every new connection would wait for the write lock at its start,
// and under steady writes a read on it failed with SQLITE_BUSY.
func initFile(ctx context.Context, dbPath string) error {
	db, err := sql.Open("sqlite", "file:"+uriPath.Replace(dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var pages int
	if err := conn.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return err
	}
	if pages > 0 {
		return nil
	}
	// auto_vacuum before WAL, whose switch writes the first page.
	for _, p := range []string{`PRAGMA auto_vacuum = INCREMENTAL`, `PRAGMA journal_mode = WAL`} {
		if _, err := conn.ExecContext(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

const createSchemaMigrations = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL
	) STRICT`

// isUnique reports a UNIQUE constraint violation.
func isUnique(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

// isBusy reports SQLITE_BUSY or SQLITE_LOCKED, including extended codes.
func isBusy(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code() & 0xff {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return true
	}
	return false
}

// uriPath escapes the characters that would otherwise end the path part of a
// file: URI ('?' starts the query, '#' a fragment) or start an escape ('%').
// SQLite decodes the %XX escapes back.
var uriPath = strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")

// WALLimit is the size the WAL file is cut back to after a checkpoint.
const WALLimit = 64 << 20

// dsn builds the modernc.org/sqlite connection string. The pragmas are set on
// every new connection of the pool, not once, and none of them writes on a
// database in WAL (a new connection must not wait for a write in progress):
// busy_timeout first, so that switching to WAL waits for a lock instead of
// failing at once when another process opens the same new file; the WAL
// file is cut back to WALLimit when it is reset, so one large body does not
// leave it at its size. A new file's auto-vacuum is set before, by initFile.
func dsn(dbPath string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_size_limit("+strconv.Itoa(WALLimit)+")")
	q.Add("_txlock", "immediate")
	return "file:" + uriPath.Replace(dbPath) + "?" + q.Encode()
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
	if _, err := db.ExecContext(ctx, createSchemaMigrations); err != nil {
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
	// Another process may have applied it since the version was read; the
	// transaction is IMMEDIATE (_txlock), so this re-read is under the lock.
	var done int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.version).Scan(&done); err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	if done > 0 {
		return nil
	}
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
func (s *Store) Close() error {
	err := s.db.Close()
	if s.lock != nil {
		s.lock.Close()
	}
	return err
}
