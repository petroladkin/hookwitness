package store

import (
	"context"
	"crypto/rand"
	"os"
	"testing"
)

// fileSize is the database file plus its WAL after a full checkpoint.
func fileSize(t *testing.T, s *Store, path string) int64 {
	t.Helper()
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	var n int64
	for _, p := range []string{path, path + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			n += fi.Size()
		}
	}
	return n
}

// fill catches n requests of 64 KiB random (incompressible) bodies.
func fill(t *testing.T, s *Store, ep int64, n, keep int) {
	t.Helper()
	for range n {
		r := sample(ep, "")
		r.Body = make([]byte, 64<<10)
		rand.Read(r.Body)
		if err := s.AddRequest(context.Background(), r, keep, nil); err != nil {
			t.Fatal(err)
		}
	}
}

// TestNewDatabaseIsIncremental: a database created by this version gives
// space back.
func TestNewDatabaseIsIncremental(t *testing.T) {
	s := openTemp(t)
	if mode, err := s.AutoVacuum(context.Background()); err != nil || mode != "incremental" {
		t.Fatalf("auto_vacuum %q (%v), want incremental", mode, err)
	}
}

// TestClearAndDeleteShrinkTheFile: clearing a register, and deleting an
// endpoint, give at least 90 % of their bodies back to the file system.
func TestClearAndDeleteShrinkTheFile(t *testing.T) {
	ctx := context.Background()
	p := tempDB(t)
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ep, _ := s.CreateEndpoint(ctx, "/big")
	other, _ := s.CreateEndpoint(ctx, "/other")
	const bodies = 128 * 64 << 10 // 8 MiB
	fill(t, s, ep.ID, 128, 1000)
	fill(t, s, other.ID, 128, 1000)
	full := fileSize(t, s, p)
	if err := s.ClearRequests(ctx, ep.ID); err != nil {
		t.Fatal(err)
	}
	cleared := fileSize(t, s, p)
	if full-cleared < bodies*9/10 {
		t.Fatalf("clearing 8 MiB of bodies shrank the file by %d bytes (%d → %d), want at least 90 %%", full-cleared, full, cleared)
	}
	if err := s.DeleteEndpoint(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	deleted := fileSize(t, s, p)
	if cleared-deleted < bodies*9/10 {
		t.Fatalf("deleting an endpoint with 8 MiB of bodies shrank the file by %d bytes, want at least 90 %%", cleared-deleted)
	}
}

// TestPruneShrinksByHousekeeping: pruned bodies are given back once enough
// is free (the housekeeping's Reclaim), in steps of at most ReclaimStep
// pages.
func TestPruneShrinksByHousekeeping(t *testing.T) {
	ctx := context.Background()
	p := tempDB(t)
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ep, _ := s.CreateEndpoint(ctx, "/pruned")
	// A flood of large bodies, then small ones that push them out: the
	// pruned pages are not reused, 10 MiB stay free.
	fill(t, s, ep.ID, 160, 160)
	for range 160 {
		s.AddRequest(ctx, sample(ep.ID, "{}"), 160, nil)
	}
	before := fileSize(t, s, p)
	if freed, _ := s.Reclaim(ctx, 64<<20); freed != 0 {
		t.Fatal("reclaimed below the threshold")
	}
	old := ReclaimStep
	ReclaimStep = 256
	defer func() { ReclaimStep = old }()
	var steps []int64
	reclaimHook = func(n int64) {
		steps = append(steps, n)
		// Between steps the writer is free: a request is caught (it would
		// wait forever if a step kept the lock).
		if len(steps) == 1 {
			if err := s.AddRequest(ctx, sample(ep.ID, "{}"), 160, nil); err != nil {
				t.Error(err)
			}
		}
	}
	defer func() { reclaimHook = nil }()
	freed, err := s.Reclaim(ctx, ReclaimFrom)
	if err != nil || freed < 9<<20 {
		t.Fatalf("freed %d (%v), want the pruned bodies", freed, err)
	}
	if len(steps) < 2 {
		t.Fatalf("one step freed everything: %v", steps)
	}
	for _, n := range steps {
		if n > 256 {
			t.Fatalf("a step freed %d pages, more than the step's 256", n)
		}
	}
	if after := fileSize(t, s, p); before-after < 9<<20 {
		t.Fatalf("file %d → %d", before, after)
	}
}

// TestReclaimLeavesAnOldDatabase: a database without incremental
// auto-vacuum (made by 0.1.0) is not touched by Reclaim.
func TestReclaimLeavesAnOldDatabase(t *testing.T) {
	ctx := context.Background()
	s := openRelease010(t)
	if mode, _ := s.AutoVacuum(ctx); mode != "none" {
		t.Fatalf("0.1.0 database is %q", mode)
	}
	s.ClearRequests(ctx, 1)
	if freed, err := s.Reclaim(ctx, 0); freed != 0 || err != nil {
		t.Fatalf("%d %v", freed, err)
	}
}

// openRelease010 opens a copy of the database the 0.1.0 server made.
func openRelease010(t *testing.T) *Store {
	t.Helper()
	src, err := os.ReadFile("../../testdata/store/v0.1.0.db")
	if err != nil {
		t.Fatal(err)
	}
	p := tempDB(t)
	os.WriteFile(p, src, 0o600)
	s, err := Open(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestClearStandsWhenTheCallerLeaves: once a clear or a delete has
// committed, the caller leaving while the space is given back neither
// fails it nor stops the giving back.
func TestClearStandsWhenTheCallerLeaves(t *testing.T) {
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(context.Background(), "/big")
	other, _ := s.CreateEndpoint(context.Background(), "/other")
	fill(t, s, ep.ID, 192, 1000) // 12 MiB each: several steps of ReclaimStep pages
	fill(t, s, other.ID, 192, 1000)
	for _, op := range []func(context.Context) error{
		func(ctx context.Context) error { return s.ClearRequests(ctx, ep.ID) },
		func(ctx context.Context) error { return s.DeleteEndpoint(ctx, other.ID) },
	} {
		ctx, cancel := context.WithCancel(context.Background())
		steps := 0
		reclaimHook = func(int64) { steps++; cancel() } // the caller leaves after the first step
		err := op(ctx)
		reclaimHook = nil
		cancel()
		if err != nil {
			t.Fatalf("committed, then answered %v", err)
		}
		if free, _ := s.freePages(context.Background()); free != 0 || steps < 2 {
			t.Fatalf("%d free pages left after %d steps: the giving back stopped with the caller", free, steps)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM requests`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d requests left (%v)", n, err)
	}
}
