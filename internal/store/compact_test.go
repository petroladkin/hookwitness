package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// dump is every row of every table of the database at path, as text, for
// comparing a database before and after.
func dump(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	var b strings.Builder
	for _, tb := range tables {
		rs, err := db.Query(`SELECT * FROM "` + tb + `" ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rs.Columns()
		n := 0
		for rs.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rs.Scan(ptrs...)
			fmt.Fprintf(&b, "%s %v\n", tb, vals)
			n++
		}
		rs.Close()
		fmt.Fprintf(&b, "%s: %d rows\n", tb, n)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("%d tables, sha256 %s", len(tables), hex.EncodeToString(sum[:8]))
}

// oldDatabase is a 0.1.0 database (auto_vacuum none) with 8 MiB of bodies
// caught and cleared: its file keeps their pages.
func oldDatabase(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	src, err := os.ReadFile(filepath.Join("..", "..", "testdata", "store", "v0.1.0.db"))
	if err != nil {
		t.Fatal(err)
	}
	p := tempDB(t)
	os.WriteFile(p, src, 0o600)
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	ep, _ := s.CreateEndpoint(ctx, "/flood")
	fill(t, s, ep.ID, 128, 1000)
	s.ClearRequests(ctx, ep.ID)
	s.Close()
	return p
}

func TestCompactConvertsAndShrinks(t *testing.T) {
	ctx := context.Background()
	p := oldDatabase(t)
	before := dump(t, p)
	res, err := Compact(ctx, p, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Converted || res.Before-res.After < 7<<20 {
		t.Fatalf("result %+v, want converted and 8 MiB smaller", res)
	}
	if after := dump(t, p); after != before {
		t.Fatalf("the data changed: %s → %s", before, after)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("the new file's mode is %v, the database's was 0600", fi.Mode().Perm())
	}
	for _, leftover := range []string{p + ".compact", p + ".before-compact"} {
		if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left behind (%v)", leftover, err)
		}
	}
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if mode, _ := s.AutoVacuum(ctx); mode != "incremental" {
		t.Fatalf("mode %q after compacting", mode)
	}
	ep, err := s.Endpoint(ctx, 1)
	if err != nil || ep.Path != "/orders" || ep.Count != 3 || ep.Config == "" {
		t.Fatalf("endpoint %+v %v", ep, err)
	}
	// A second run finds nothing to convert.
	s.Close()
	if res, err := Compact(ctx, p, false); err != nil || res.Converted {
		t.Fatalf("again: %+v %v", res, err)
	}
}

// TestCompactSyncsBeforeTheRename: the copy reaches the disk before it
// becomes the database, and the directory after.
func TestCompactSyncsBeforeTheRename(t *testing.T) {
	p := oldDatabase(t)
	var steps []string
	compactStep = func(s string) { steps = append(steps, s) }
	defer func() { compactStep = nil }()
	if _, err := Compact(context.Background(), p, true); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(steps, ", "); got != "sync file, rename, sync dir" {
		t.Fatalf("steps %q", got)
	}
}

func TestCompactKeepsTheOldFile(t *testing.T) {
	ctx := context.Background()
	p := oldDatabase(t)
	before := dump(t, p)
	if _, err := Compact(ctx, p, true); err != nil {
		t.Fatal(err)
	}
	if got := dump(t, p+".before-compact"); got != before {
		t.Fatalf("the kept file differs: %s, want %s", got, before)
	}
	if _, err := Compact(ctx, p, true); err == nil || !strings.Contains(err.Error(), "before-compact") {
		t.Fatalf("an old file in the way must stop it: %v", err)
	}
}

// TestCompactRefusesWhileOpen: not while a server (or any process) has the
// database open, and nothing opens it while it is being compacted.
func TestCompactRefusesWhileOpen(t *testing.T) {
	ctx := context.Background()
	p := oldDatabase(t)
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Compact(ctx, p, false); !errors.Is(err, ErrInUse) {
		t.Fatalf("compacting an open database: %v", err)
	}
	s.Close()
	lock, err := lockFile(p+".lock", true) // as Compact holds it
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := Open(ctx, p); !errors.Is(err, ErrInUse) {
		t.Fatalf("opening a database being compacted: %v", err)
	}
}

// TestCompactFailureLeavesTheDatabase: a rewrite that cannot be written
// (here: no permission where it goes; likewise a full disk) changes
// nothing and says why.
func TestCompactFailureLeavesTheDatabase(t *testing.T) {
	ctx := context.Background()
	p := oldDatabase(t)
	before := dump(t, p)
	ro := t.TempDir()
	os.Chmod(ro, 0o500)
	defer os.Chmod(ro, 0o700)
	old := compactTemp
	compactTemp = func(string) string { return filepath.Join(ro, "hw.db.compact") }
	defer func() { compactTemp = old }()
	_, err := Compact(ctx, p, false)
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("error %v", err)
	}
	if after := dump(t, p); after != before {
		t.Fatal("the database changed")
	}
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if mode, _ := s.AutoVacuum(ctx); mode != "none" {
		t.Fatalf("mode %q", mode)
	}
}

// A new lock file takes the database's mode (and owner), and a lock file
// this user cannot write is still taken, read-only: a lock left by another
// user (sudo hookwitness db compact) does not keep the server out.
func TestLockFileOwnerAndReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no advisory locks here")
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "h.db")
	if err := os.WriteFile(db, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	f, err := lockFile(db+".lock", false)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if fi, _ := os.Stat(db + ".lock"); fi.Mode().Perm() != 0o640 {
		t.Fatalf("lock mode %v, want the database's 0640", fi.Mode().Perm())
	}
	if err := os.Chmod(db+".lock", 0o444); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes any file")
	}
	f, err = lockFile(db+".lock", false)
	if err != nil {
		t.Fatalf("a read-only lock file: %v", err)
	}
	if _, err := lockFile(db+".lock", true); !errors.Is(err, errLocked) {
		t.Fatalf("exclusive while shared is held: %v", err)
	}
	f.Close()
}
