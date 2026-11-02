package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sampleForward(reqID int64, status int) *Forward {
	return &Forward{RequestID: reqID, CreatedAt: time.Now(), URL: "http://receiver.test/in", Options: `{"host":"target"}`,
		Line: "POST /in HTTP/1.1", Sent: []Header{{"Host", "receiver.test"}, {"X-Sig", "abc"}}, Notes: []string{"n1"},
		Proto: "HTTP/1.1", Status: status, Reason: "OK", Headers: []Header{{"set-cookie", "a=1"}, {"Set-Cookie", "b=2"}},
		Body: []byte{0xff, 'o', 'k'}, Size: 3, Duration: 38 * time.Millisecond, TTFB: 12 * time.Millisecond}
}

const forwardedOK = `{"source":"forward","action":"forward","status":200,"forward":{"target":"http://receiver.test/in","status":200,"ms":38}}`

// TestFinishForward: the exchange and the final outcome are stored
// together, the request takes the next change sequence and its lane
// follows; read back whole, header order and case kept.
func TestFinishForward(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/a")
	r := sample(ep.ID, "body")
	r.Outcome = `{"source":"forward","action":"forward","forward":{"target":"http://receiver.test/in","pending":true,"ms":0}}`
	if err := s.AddRequest(ctx, r, Keep{Normal: 100, Anomalies: 100}, nil); err != nil {
		t.Fatal(err)
	}
	var anomaly bool
	var seq0 int64
	s.db.QueryRowContext(ctx, `SELECT anomaly, change_seq FROM requests WHERE id = ?`, r.ID).Scan(&anomaly, &seq0)
	if !anomaly {
		t.Fatal("a pending forward is not yet a good answer")
	}
	fw := sampleForward(r.ID, 200)
	if err := s.FinishForward(ctx, fw, 200, forwardedOK); err != nil {
		t.Fatal(err)
	}
	var seq int64
	s.db.QueryRowContext(ctx, `SELECT anomaly, change_seq FROM requests WHERE id = ?`, r.ID).Scan(&anomaly, &seq)
	if anomaly || seq <= seq0 || fw.Register != ep.ID {
		t.Fatalf("after the forward: anomaly %v, seq %d (was %d), register %d", anomaly, seq, seq0, fw.Register)
	}
	got, err := s.ForwardOf(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Line != fw.Line || len(got.Sent) != 2 || got.Headers[0].Name != "set-cookie" || got.Headers[1].Name != "Set-Cookie" ||
		string(got.Body) != "\xffok" || got.Duration != 38*time.Millisecond || got.TTFB != 12*time.Millisecond || got.Notes[0] != "n1" {
		t.Fatalf("read back %+v", got)
	}
	if req, _ := s.Request(ctx, r.ID); req.Status != 200 || req.Outcome != forwardedOK {
		t.Fatalf("request after the forward: %d %s", req.Status, req.Outcome)
	}
	if _, err := s.ForwardOf(ctx, 999); err != ErrNotFound {
		t.Fatalf("no forward: %v", err)
	}
	// A request pruned while it was forwarded: nothing is stored.
	if err := s.FinishForward(ctx, sampleForward(999, 200), 200, forwardedOK); err != ErrNotFound {
		t.Fatalf("a gone request: %v", err)
	}
}

// TestForwardsGoWithTheirRequest: clear, delete and pruning remove them.
func TestForwardsGoWithTheirRequest(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	count := func() int {
		var n int
		s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM forwards`).Scan(&n)
		return n
	}
	ep, _ := s.CreateEndpoint(ctx, "/a")
	add := func(keep int) {
		r := sample(ep.ID, "b")
		if err := s.AddRequest(ctx, r, Keep{Normal: keep, Anomalies: keep}, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishForward(ctx, sampleForward(r.ID, 200), 200, forwardedOK); err != nil {
			t.Fatal(err)
		}
	}
	add(100)
	add(100)
	if err := s.ClearRequests(ctx, ep.ID); err != nil || count() != 0 {
		t.Fatalf("after clear: %d forwards, %v", count(), err)
	}
	add(1)
	add(1) // prunes the first
	if count() != 1 {
		t.Fatalf("after pruning: %d forwards", count())
	}
	if err := s.DeleteEndpoint(ctx, ep.ID); err != nil || count() != 0 {
		t.Fatalf("after deleting the endpoint: %d forwards, %v", count(), err)
	}
}

// TestForwardFilter: ok, failed and none read the outcome's receiver note.
func TestForwardFilter(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/a")
	outcomes := map[string]string{
		"ok":      forwardedOK,
		"non2xx":  `{"source":"forward","action":"forward","status":409,"forward":{"target":"t","status":409,"ms":1}}`,
		"down":    `{"source":"forward","action":"forward","status":502,"forward":{"target":"t","error_kind":"dial","fallback":true,"ms":1}}`,
		"pending": `{"source":"forward","action":"forward","forward":{"target":"t","pending":true,"ms":0}}`,
		"too":     `{"source":"rule","rule":1,"action":"reply","status":500,"forward":{"target":"t","status":200,"ms":1}}`,
		"kept":    `{"source":"rule","rule":1,"action":"reply","status":500,"not_forwarded":true}`,
		"mock":    `{"source":"default","action":"reply","status":200}`,
		"old":     ``,
	}
	ids := map[string]int64{}
	for name, o := range outcomes {
		r := sample(ep.ID, name)
		r.Outcome = o
		if err := s.AddRequest(ctx, r, Keep{Normal: 100, Anomalies: 100}, nil); err != nil {
			t.Fatal(err)
		}
		ids[name] = r.ID
	}
	for filter, want := range map[string][]string{
		ForwardOK:     {"ok", "too"},
		ForwardFailed: {"non2xx", "down"},
		ForwardNone:   {"kept", "mock", "old"},
	} {
		got, err := s.List(ctx, ep.ID, ListQuery{Forward: filter})
		if err != nil {
			t.Fatal(err)
		}
		names := map[int64]bool{}
		for _, r := range got {
			names[r.ID] = true
		}
		if len(got) != len(want) {
			t.Errorf("%s: %d requests, want %v", filter, len(got), want)
		}
		for _, n := range want {
			if !names[ids[n]] {
				t.Errorf("%s: %s missing", filter, n)
			}
		}
	}
}

// AC-13: a 0.2.0 database opens, keeps its data, and its requests can get
// forwards.
func TestForwardsOnRelease020Database(t *testing.T) {
	ctx := context.Background()
	src, err := os.ReadFile(filepath.Join("..", "..", "testdata", "store", "v0.2.0.db"))
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
	ep, err := s.Endpoint(ctx, 1)
	if err != nil || ep.Path != "/orders" || ep.Counter != 3 || ep.Count != 3 || !ep.StartedAt.IsZero() {
		t.Fatalf("the old endpoint: %+v, %v", ep, err)
	}
	reqs, err := s.Requests(ctx, 1, 0, 10)
	if err != nil || len(reqs) != 3 || reqs[2].Status != 503 || reqs[0].Status != 202 || reqs[0].Verdict != "pass" {
		t.Fatalf("old requests %+v, %v", reqs, err)
	}
	if un, _ := s.Requests(ctx, Unmatched, 0, 10); len(un) != 1 || un[0].Path != "/nowhere" {
		t.Fatalf("old Unmatched %+v", un)
	}
	if err := s.FinishForward(ctx, sampleForward(reqs[0].ID, 200), 200, forwardedOK); err != nil {
		t.Fatal(err)
	}
	if fw, err := s.ForwardOf(ctx, reqs[0].ID); err != nil || fw.Status != 200 {
		t.Fatalf("a forward of an old request: %+v, %v", fw, err)
	}
	if v, _ := s.SchemaVersion(ctx); v != 13 {
		t.Fatalf("schema %d after the upgrade", v)
	}
}
