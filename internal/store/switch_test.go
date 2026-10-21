package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A database made by the 0.1.0 server opens, with its data kept and every
// endpoint on.
func TestOpensRelease010Database(t *testing.T) {
	ctx := context.Background()
	src, err := os.ReadFile(filepath.Join("..", "..", "testdata", "store", "v0.1.0.db"))
	if err != nil {
		t.Fatal(err)
	}
	p := tempDB(t)
	if err := os.WriteFile(p, src, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want, _ := newest(t)
	if v, _ := s.SchemaVersion(ctx); v != want {
		t.Fatalf("schema %d after opening, want %d", v, want)
	}
	ep, err := s.Endpoint(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ep.Path != "/orders" || ep.Count != 3 || ep.Counter != 3 || !ep.Enabled {
		t.Fatalf("endpoint after the migration: %+v", ep)
	}
	if ep.Config == "" {
		t.Fatal("the behaviour was lost")
	}
	un, err := s.Endpoint(ctx, Unmatched)
	if err != nil || un.Count != 1 {
		t.Fatalf("Unmatched after the migration: %+v, %v", un, err)
	}
	// It still catches: the next request is number 4, counter 4.
	r := sample(1, `{"order":4}`)
	if err := s.AddRequest(ctx, r, 100, func(n int64) (int, string) { return 202, "" }); err != nil {
		t.Fatal(err)
	}
	if r.No != 4 {
		t.Fatalf("next number %d, want 4", r.No)
	}
	if ep, _ = s.Endpoint(ctx, 1); ep.Counter != 4 {
		t.Fatalf("counter %d, want 4", ep.Counter)
	}
}

func TestEndpointOnAndOff(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, err := s.CreateEndpoint(ctx, "/a")
	if err != nil {
		t.Fatal(err)
	}
	if !ep.Enabled {
		t.Fatal("a new endpoint is off")
	}
	if err := s.SetConfig(ctx, ep.ID, `{"x":1}`); err != nil {
		t.Fatal(err)
	}
	if err := s.AddRequest(ctx, sample(ep.ID, "1"), 100, func(int64) (int, string) { return 200, "" }); err != nil {
		t.Fatal(err)
	}

	off := false
	if err := s.UpdateEndpoint(ctx, ep.ID, EndpointChange{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Endpoint(ctx, ep.ID)
	if got.Enabled || got.Path != "/a" || got.Config != `{"x":1}` || got.Count != 1 || got.Counter != 1 {
		t.Fatalf("off endpoint: %+v", got)
	}
	id, cfg, on, err := s.EndpointByPath(ctx, "/a")
	if err != nil || id != ep.ID || cfg != `{"x":1}` || on {
		t.Fatalf("EndpointByPath of an off endpoint: %d %q %v %v", id, cfg, on, err)
	}
	// Its path stays taken.
	if _, err := s.CreateEndpoint(ctx, "/a"); !errors.Is(err, ErrDuplicatePath) {
		t.Fatalf("creating at an off endpoint's path: %v", err)
	}
	// A request decided for it while it was on is refused once it is off.
	if err := s.AddRequest(ctx, sample(ep.ID, "2"), 100, func(int64) (int, string) { return 200, "" }); !errors.Is(err, ErrOff) {
		t.Fatalf("AddRequest to an off endpoint: %v", err)
	}
	if got, _ := s.Endpoint(ctx, ep.ID); got.Count != 1 || got.Counter != 1 {
		t.Fatalf("the refused request changed the endpoint: %+v", got)
	}

	on = true
	if err := s.UpdateEndpoint(ctx, ep.ID, EndpointChange{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	r := sample(ep.ID, "3")
	if err := s.AddRequest(ctx, r, 100, func(n int64) (int, string) { return 200, "" }); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Endpoint(ctx, ep.ID); !got.Enabled || got.Counter != 2 || r.No != 2 {
		t.Fatalf("back on: %+v, no %d", got, r.No)
	}
}

func TestUpdateEndpointPathAndStateTogether(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	a, _ := s.CreateEndpoint(ctx, "/a")
	if _, err := s.CreateEndpoint(ctx, "/b"); err != nil {
		t.Fatal(err)
	}
	off, taken, moved := false, "/b", "/c"
	// A taken path changes nothing, the state included.
	if err := s.UpdateEndpoint(ctx, a.ID, EndpointChange{Path: &taken, Enabled: &off}); !errors.Is(err, ErrDuplicatePath) {
		t.Fatalf("taken path: %v", err)
	}
	if got, _ := s.Endpoint(ctx, a.ID); !got.Enabled || got.Path != "/a" {
		t.Fatalf("after a refused change: %+v", got)
	}
	if err := s.UpdateEndpoint(ctx, a.ID, EndpointChange{Path: &moved, Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Endpoint(ctx, a.ID); got.Enabled || got.Path != "/c" {
		t.Fatalf("after moving and turning off: %+v", got)
	}
	if err := s.UpdateEndpoint(ctx, 999, EndpointChange{Enabled: &off}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing endpoint: %v", err)
	}
	// Nothing to change is not an error.
	if err := s.UpdateEndpoint(ctx, a.ID, EndpointChange{}); err != nil {
		t.Fatal(err)
	}
	eps, _ := s.Endpoints(ctx)
	if len(eps) != 2 || eps[1].Path != "/c" || eps[1].Enabled || !eps[0].Enabled {
		t.Fatalf("list: %+v", eps)
	}
}
