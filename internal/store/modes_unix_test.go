//go:build unix

package store

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// withUmask runs the test under a usual umask (022), which would leave a
// file created without a mode readable by everyone.
func withUmask(t *testing.T) {
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
}

// A new database and its -wal and -shm are private; an existing database
// keeps the mode it has.
func TestNewDatabaseIsPrivate(t *testing.T) {
	withUmask(t)
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "new.db")
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateEndpoint(ctx, "/a"); err != nil { // writes, so the WAL exists
		t.Fatal(err)
	}
	for _, f := range []string{p, p + "-wal", p + "-shm"} {
		if m := mode(t, f); m != 0o600 {
			t.Errorf("%s: %v, want -rw-------", filepath.Base(f), m)
		}
	}
	s.Close()

	old := filepath.Join(dir, "old.db")
	s, err = Open(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	os.Chmod(old, 0o644)
	s, err = Open(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if m := mode(t, old); m != 0o644 {
		t.Fatalf("an existing database's mode changed: %v", m)
	}
}

// While db compact writes and checks the copy, the copy is private; done,
// it has the database's own mode, whatever that is.
func TestCompactCopyIsPrivate(t *testing.T) {
	withUmask(t)
	ctx := context.Background()
	for _, dbMode := range []os.FileMode{0o600, 0o640} {
		p := filepath.Join(t.TempDir(), "hw.db")
		s, err := Open(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		ep, _ := s.CreateEndpoint(ctx, "/a")
		s.AddRequest(ctx, sample(ep.ID, "secret body"), Keep{Normal: 10}, nil)
		s.Close()
		os.Chmod(p, dbMode)
		var during os.FileMode
		afterCopy = func(tmp string) { during = mode(t, tmp) }
		_, err = Compact(ctx, p, false)
		afterCopy = nil
		if err != nil {
			t.Fatal(err)
		}
		if during != 0o600 {
			t.Errorf("database %v: the copy was %v while written and checked, want -rw-------", dbMode, during)
		}
		if m := mode(t, p); m != dbMode {
			t.Errorf("database %v: compacted to %v", dbMode, m)
		}
	}
}
