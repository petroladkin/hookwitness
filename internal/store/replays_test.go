package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sampleReplay(reqID int64, status int) *Replay {
	return &Replay{RequestID: reqID, CreatedAt: time.Now(), By: "ada", URL: "http://127.0.0.1:3000/hook?x=1",
		Options: `{"host":"target"}`, Line: "POST /hook HTTP/1.1",
		Sent:  []Header{{"Host", "127.0.0.1:3000"}, {"x-sig", "1"}},
		Notes: []string{"n"}, Proto: "HTTP/1.1", Status: status, Reason: "OK",
		Headers: []Header{{"x-b", "1"}, {"X-A", "2"}}, Body: []byte{0xff, 'o', 'k'}, Size: 3,
		Duration: 38 * time.Millisecond, TTFB: 12 * time.Millisecond}
}

func TestReplays(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/a")
	r := sample(ep.ID, "body")
	if err := s.AddRequest(ctx, r, 100, nil); err != nil {
		t.Fatal(err)
	}
	first := sampleReplay(r.ID, 200)
	if err := s.AddReplay(ctx, first); err != nil {
		t.Fatal(err)
	}
	failed := &Replay{RequestID: r.ID, CreatedAt: time.Now(), URL: "http://127.0.0.1:1/", Options: "{}", Line: "POST /a HTTP/1.1",
		ErrorKind: "dial", Error: "connection refused"}
	if err := s.AddReplay(ctx, failed); err != nil {
		t.Fatal(err)
	}
	got, err := s.Replay(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestID != r.ID || got.By != "ada" || got.Status != 200 || string(got.Body) != "\xffok" ||
		fmt.Sprint(got.Headers) != "[{x-b 1} {X-A 2}]" || fmt.Sprint(got.Sent) != "[{Host 127.0.0.1:3000} {x-sig 1}]" ||
		got.Duration != 38*time.Millisecond || got.TTFB != 12*time.Millisecond || len(got.Notes) != 1 || got.Line != "POST /hook HTTP/1.1" {
		t.Fatalf("replay %+v", got)
	}
	list, err := s.Replays(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != failed.ID || list[0].Error != "connection refused" || list[1].Body != nil {
		t.Fatalf("list (newest first, no bodies) %+v", list)
	}
	if req, _ := s.Request(ctx, r.ID); req.Replays != 2 {
		t.Fatalf("the request counts %d replays", req.Replays)
	}
	if reqs, _ := s.Requests(ctx, ep.ID, 0, 10); reqs[0].Replays != 2 {
		t.Fatalf("a list counts %d replays", reqs[0].Replays)
	}
	if _, err := s.Replay(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing replay: %v", err)
	}
	if err := s.AddReplay(ctx, sampleReplay(9999, 200)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a replay of a request that is gone: %v", err)
	}
}

// The newest 50 replays of a request are kept.
func TestReplayCap(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/a")
	r := sample(ep.ID, "body")
	s.AddRequest(ctx, r, 100, nil)
	var ids []int64
	for i := range MaxReplays + 1 {
		rp := sampleReplay(r.ID, 200+i)
		if err := s.AddReplay(ctx, rp); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rp.ID)
	}
	list, _ := s.Replays(ctx, r.ID)
	if len(list) != MaxReplays || list[0].ID != ids[MaxReplays] || list[MaxReplays-1].ID != ids[1] {
		t.Fatalf("kept %d, newest %d, oldest %d", len(list), list[0].ID, list[len(list)-1].ID)
	}
	if _, err := s.Replay(ctx, ids[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the 51st replay did not drop the oldest: %v", err)
	}
}

// Replays go with their request: clear, pruning and deleting the endpoint.
func TestReplaysGoWithTheirRequest(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	count := func() int {
		var n int
		s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM replays`).Scan(&n)
		return n
	}
	ep, _ := s.CreateEndpoint(ctx, "/a")
	add := func(keep int) *Request {
		r := sample(ep.ID, "b")
		if err := s.AddRequest(ctx, r, keep, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.AddReplay(ctx, sampleReplay(r.ID, 200)); err != nil {
			t.Fatal(err)
		}
		return r
	}
	add(100)
	add(100)
	if err := s.ClearRequests(ctx, ep.ID); err != nil || count() != 0 {
		t.Fatalf("after clear: %d replays, %v", count(), err)
	}
	add(1)
	add(1) // prunes the first
	if count() != 1 {
		t.Fatalf("after pruning: %d replays", count())
	}
	if err := s.DeleteEndpoint(ctx, ep.ID); err != nil || count() != 0 {
		t.Fatalf("after deleting the endpoint: %d replays, %v", count(), err)
	}
	// The Unmatched register too.
	u := sample(Unmatched, "u")
	s.AddRequest(ctx, u, 100, nil)
	s.AddReplay(ctx, sampleReplay(u.ID, 404))
	if err := s.ClearRequests(ctx, Unmatched); err != nil || count() != 0 {
		t.Fatalf("after clearing Unmatched: %d replays, %v", count(), err)
	}
}

// A 0.1.0 database gains the table, and its old requests can be replayed.
func TestReplaysOnRelease010Database(t *testing.T) {
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
	defer s.Close()
	reqs, err := s.Requests(ctx, 1, 0, 10)
	if err != nil || len(reqs) != 3 {
		t.Fatalf("old requests %d, %v", len(reqs), err)
	}
	if err := s.AddReplay(ctx, sampleReplay(reqs[0].ID, 200)); err != nil {
		t.Fatal(err)
	}
	if req, _ := s.Request(ctx, reqs[0].ID); req.Replays != 1 || len(req.Body) == 0 {
		t.Fatalf("old request after a replay: %+v", req)
	}
}
