package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func tempDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "hw.db")
}

func newest(t *testing.T) (version, count int) {
	t.Helper()
	ms, err := loadMigrations(migrationFS)
	if err != nil {
		t.Fatal(err)
	}
	return ms[len(ms)-1].version, len(ms)
}

func TestOpenCreatesAndMigrates(t *testing.T) {
	ctx := context.Background()
	p := tempDB(t)
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("database file not created: %v", err)
	}
	v, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := newest(t); v != want {
		t.Fatalf("schema version %d, want %d", v, want)
	}
}

func TestPragmas(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, tempDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for pragma, want := range map[string]string{"journal_mode": "wal", "busy_timeout": "5000", "foreign_keys": "1"} {
		var got string
		if err := s.db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("PRAGMA %s = %s, want %s", pragma, got, want)
		}
	}
}

func TestReopenIsNoop(t *testing.T) {
	ctx := context.Background()
	p := tempDB(t)
	_, count := newest(t)
	for i := 1; i <= 2; i++ {
		s, err := Open(ctx, p)
		if err != nil {
			t.Fatalf("open #%d: %v", i, err)
		}
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != count {
			t.Fatalf("open #%d: %d migration rows, want %d", i, n, count)
		}
		s.Close()
	}
}

func TestMigrationsApplyInOrder(t *testing.T) {
	ctx := context.Background()
	fsys := fstest.MapFS{
		"migrations/0002_b.sql": {Data: []byte(`INSERT INTO t (v) VALUES ('second');`)},
		"migrations/0001_a.sql": {Data: []byte(`CREATE TABLE t (v TEXT); INSERT INTO t (v) VALUES ('first');`)},
		"migrations/0010_c.sql": {Data: []byte(`INSERT INTO t (v) VALUES ('tenth');`)},
		"migrations/README.txt": {Data: []byte(`ignored`)},
	}
	s, err := open(ctx, tempDB(t), fsys)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.db.QueryContext(ctx, `SELECT v FROM t ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	if strings.Join(got, ",") != "first,second,tenth" {
		t.Fatalf("applied in order %v", got)
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	ctx := context.Background()
	first := &fstest.MapFile{Data: []byte(`CREATE TABLE t (v TEXT);`)}
	fsys := fstest.MapFS{
		"migrations/0001_a.sql": first,
		"migrations/0002_b.sql": {Data: []byte(`CREATE TABLE u (v TEXT); SELECT * FROM missing;`)},
	}
	p := tempDB(t)
	if _, err := open(ctx, p, fsys); err == nil || !strings.Contains(err.Error(), "0002_b.sql") {
		t.Fatalf("want an error naming 0002_b.sql, got %v", err)
	}
	s, err := open(ctx, p, fstest.MapFS{"migrations/0001_a.sql": first})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'u'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("table from the failed migration survived")
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	ctx := context.Background()
	p := tempDB(t)
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations VALUES (9999, '9999_future.sql', '2030-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	_, err = Open(ctx, p)
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("want ErrSchemaTooNew, got %v", err)
	}
	if !strings.Contains(err.Error(), "9999") {
		t.Fatalf("error should name the database version: %v", err)
	}
}

func TestBadMigrationNames(t *testing.T) {
	for name, fsys := range map[string]fstest.MapFS{
		"no number": {"migrations/init.sql": {Data: []byte(`SELECT 1;`)}},
		"duplicate": {
			"migrations/0001_a.sql": {Data: []byte(`SELECT 1;`)},
			"migrations/0001_b.sql": {Data: []byte(`SELECT 1;`)},
		},
	} {
		if _, err := loadMigrations(fsys); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// The path reaches SQLite as a file: URI, where '?', '#' and '%' mean
// something; the file must still be created under exactly the given name.
func TestPathWithURICharacters(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "dir with space")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "a#b?c%41.db")
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("database not at %q; directory holds %v", p, names)
	}
}
