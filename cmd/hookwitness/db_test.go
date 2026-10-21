package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/petroladkin/hookwitness/internal/store"
)

// release010 is a copy of the database the 0.1.0 server made.
func release010(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "testdata", "store", "v0.1.0.db"))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "hw.db")
	if err := os.WriteFile(p, src, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDBCompact(t *testing.T) {
	ctx := context.Background()
	db := release010(t)
	env := func(string) string { return "" }

	// While anything has it open: refused, unchanged.
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := run(ctx, []string{"db", "compact", "-db", db}, env, &out, &errs); code != 1 || !strings.Contains(errs.String(), "stop it first") {
		t.Fatalf("exit %d, %q", code, errs.String())
	}
	st.Close()

	out.Reset()
	errs.Reset()
	if code := run(ctx, []string{"db", "compact", "-db", db}, env, &out, &errs); code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	if !strings.Contains(out.String(), db+" converted and compacted: ") {
		t.Fatalf("output %q", out.String())
	}
	st, err = store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if mode, _ := st.AutoVacuum(ctx); mode != "incremental" {
		t.Fatalf("mode %q", mode)
	}
	if ep, err := st.Endpoint(ctx, 1); err != nil || ep.Path != "/orders" {
		t.Fatalf("%+v %v", ep, err)
	}
}

func TestDBUsage(t *testing.T) {
	ctx := context.Background()
	var out, errs bytes.Buffer
	for _, args := range [][]string{{"db"}, {"db", "shrink"}, {"db", "compact", "-nope"}, {"db", "compact", "x"}} {
		if code := run(ctx, args, func(string) string { return "" }, &out, &errs); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if code := run(ctx, []string{"db", "compact", "-db", filepath.Join(t.TempDir(), "none.db")}, func(string) string { return "" }, &out, &errs); code != 1 {
		t.Errorf("a missing database: exit %d, want 1", code)
	}
}

// TestServeHintsCompact: a database of an older version is named once at
// start with the command that converts it; a new one is not.
func TestServeHintsCompact(t *testing.T) {
	for _, c := range []struct {
		name string
		db   string
		hint bool
	}{
		{"0.1.0", release010(t), true},
		{"new", filepath.Join(t.TempDir(), "hw.db"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, log, stop, _ := serveWith(t, c.db, nil)
			stop()
			if n := strings.Count(log.String(), "run hookwitness db compact"); n != map[bool]int{true: 1, false: 0}[c.hint] {
				t.Fatalf("hint %d times; log: %s", n, log.String())
			}
		})
	}
}
