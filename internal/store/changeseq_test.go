package store

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestChangeSeqOrdersUpdates: outcome and verdict updates take the next
// value of one store-wide sequence; RequestsChanged selects by it, never by
// the clock, so an update stamped with an earlier time is still listed.
func TestChangeSeqOrdersUpdates(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/seq")
	a, b := sample(ep.ID, "a"), sample(ep.ID, "b")
	s.AddRequest(ctx, a, 100, nil)
	s.AddRequest(ctx, b, 100, nil)
	start, err := s.ChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateOutcome(ctx, b.ID, 504, `{"action":"timeout"}`); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVerdicts(ctx, a.ID, "fail", `[{"result":"fail"}]`, nil, nil); err != nil {
		t.Fatal(err)
	}
	// The clocks disagree with the commit order: a's change is stamped
	// before b's. The sequence does not care.
	s.db.ExecContext(ctx, `UPDATE requests SET changed_at = 1 WHERE id = ?`, a.ID)
	s.db.ExecContext(ctx, `UPDATE requests SET changed_at = 2 WHERE id = ?`, b.ID)
	changed, seq, err := s.RequestsChanged(ctx, ep.ID, b.ID, start)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 2 || seq != start+2 {
		t.Fatalf("changed %d rows up to seq %d, want both up to %d", len(changed), seq, start+2)
	}
	if changed[0].ID != a.ID || changed[1].ID != b.ID {
		t.Fatal("changed rows are not in id order")
	}
	// After the watermark nothing is listed; one more change is.
	if again, s2, _ := s.RequestsChanged(ctx, ep.ID, b.ID, seq); len(again) != 0 || s2 != seq {
		t.Fatalf("after the watermark: %d rows, seq %d", len(again), s2)
	}
	s.UpdateOutcome(ctx, a.ID, 200, `{}`)
	if again, s2, _ := s.RequestsChanged(ctx, ep.ID, b.ID, seq); len(again) != 1 || again[0].ID != a.ID || s2 != seq+1 {
		t.Fatalf("one more change: %d rows, seq %d", len(again), s2)
	}
	// Rows past upToID are not updates (the feed sends them as new rows).
	if some, _, _ := s.RequestsChanged(ctx, ep.ID, a.ID, start); len(some) != 1 || some[0].ID != a.ID {
		t.Fatalf("up to a: %+v", some)
	}
}

// TestChangeSeqConcurrentWriters: concurrent updates each get their own
// sequence value; none is lost.
func TestChangeSeqConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/race")
	var reqs []*Request
	for i := range 20 {
		r := sample(ep.ID, string(rune('a'+i)))
		s.AddRequest(ctx, r, 100, nil)
		reqs = append(reqs, r)
	}
	start, _ := s.ChangeSeq(ctx)
	var wg sync.WaitGroup
	for i, r := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				s.UpdateOutcome(ctx, r.ID, 200, `{}`)
			} else {
				s.SetVerdicts(ctx, r.ID, "pass", `[]`, nil, nil)
			}
		}()
	}
	wg.Wait()
	changed, seq, err := s.RequestsChanged(ctx, ep.ID, reqs[len(reqs)-1].ID, start)
	if err != nil || len(changed) != 20 || seq != start+20 {
		t.Fatalf("%d rows up to %d (%v), want 20 up to %d", len(changed), seq, err, start+20)
	}
}

// TestChangeSeqFromRelease010: the 0.1.0 database gets the sequence at 0,
// its rows list as unchanged, and the next update is listed.
func TestChangeSeqFromRelease010(t *testing.T) {
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
	if seq, err := s.ChangeSeq(ctx); err != nil || seq != 0 {
		t.Fatalf("sequence %d (%v) on a migrated database, want 0", seq, err)
	}
	old, _ := s.Requests(ctx, 1, 0, 10)
	if len(old) != 3 {
		t.Fatalf("%d requests kept, want 3", len(old))
	}
	if changed, _, _ := s.RequestsChanged(ctx, 1, old[0].ID, 0); len(changed) != 0 {
		t.Fatalf("rows of 0.1.0 listed as changed: %d", len(changed))
	}
	s.UpdateOutcome(ctx, old[0].ID, 500, `{}`)
	if changed, seq, _ := s.RequestsChanged(ctx, 1, old[0].ID, 0); len(changed) != 1 || seq != 1 {
		t.Fatalf("after one update: %d rows, seq %d", len(changed), seq)
	}
}
