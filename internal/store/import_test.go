package store

import (
	"context"
	"errors"
	"testing"
)

func TestImportEndpoints(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	old, err := s.CreateEndpoint(ctx, "/orders")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetConfig(ctx, old.ID, `{"old":1}`); err != nil {
		t.Fatal(err)
	}
	off := false
	if err := s.UpdateEndpoint(ctx, old.ID, EndpointChange{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	set := func(cfg string) func(string, bool) (string, error) {
		return func(string, bool) (string, error) { return cfg, nil }
	}

	// A taken path without Replace: nothing is written, the new one neither.
	_, err = s.ImportEndpoints(ctx, []ImportItem{{Path: "/new", Config: set(`{"n":1}`)}, {Path: "/orders", Config: set(`{"o":2}`)}})
	if !errors.Is(err, ErrDuplicatePath) {
		t.Fatalf("taken path: %v", err)
	}
	if _, _, _, err := s.EndpointByPath(ctx, "/new"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("/new was created by a refused import: %v", err)
	}

	// A failing config function rolls everything back too.
	boom := errors.New("boom")
	_, err = s.ImportEndpoints(ctx, []ImportItem{{Path: "/new", Config: set(`{"n":1}`)},
		{Path: "/orders", Replace: true, Config: func(string, bool) (string, error) { return "", boom }}})
	if !errors.Is(err, boom) {
		t.Fatalf("config error: %v", err)
	}
	if _, _, _, err := s.EndpointByPath(ctx, "/new"); !errors.Is(err, ErrNotFound) {
		t.Fatal("/new survived a failed import")
	}

	var seen string
	res, err := s.ImportEndpoints(ctx, []ImportItem{
		{Path: "/new", Config: set(`{"n":1}`)},
		{Path: "/orders", Replace: true, Config: func(o string, exists bool) (string, error) {
			if !exists {
				t.Error("existing endpoint reported as new")
			}
			seen = o
			return `{"o":2}`, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != `{"old":1}` {
		t.Errorf("the replace saw %q", seen)
	}
	if len(res) != 2 || !res[0].Created || !res[0].Changed || res[1].Created || !res[1].Changed || res[1].ID != old.ID {
		t.Fatalf("results %+v", res)
	}
	ep, _ := s.Endpoint(ctx, old.ID)
	if ep.Config != `{"o":2}` || ep.Enabled {
		t.Fatalf("replaced endpoint %+v: config replaced, still off", ep)
	}
	nw, _ := s.Endpoint(ctx, res[0].ID)
	if nw.Config != `{"n":1}` || !nw.Enabled || nw.Path != "/new" {
		t.Fatalf("new endpoint %+v", nw)
	}

	// The same again changes nothing.
	res, err = s.ImportEndpoints(ctx, []ImportItem{{Path: "/new", Replace: true, Config: set(`{"n":1}`)}})
	if err != nil || res[0].Changed || res[0].Created {
		t.Fatalf("second import: %+v %v", res, err)
	}
}
