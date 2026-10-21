package store

import (
	"context"
	"fmt"
	"testing"
)

// noted returns a Repeated func recording what it was given.
func noted(got *map[int]int64) func(map[int]int64) string {
	return func(e map[int]int64) string {
		*got = e
		return fmt.Sprint("noted ", e)
	}
}

// TestSignatureRepeats: a request carrying a signature an earlier kept
// request of the same endpoint carried learns that request's number; other
// endpoints, other signatures, pruned and cleared requests do not count.
func TestSignatureRepeats(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/sig")
	other, _ := s.CreateEndpoint(ctx, "/other")
	add := func(endpoint int64, keep int, sigs ...Signature) (*Request, map[int]int64) {
		t.Helper()
		var got map[int]int64
		r := sample(endpoint, "{}")
		r.Verdicts = "as verified"
		r.Signatures, r.Repeated = sigs, noted(&got)
		if err := s.AddRequest(ctx, r, keep, nil); err != nil {
			t.Fatal(err)
		}
		return r, got
	}
	first, got := add(ep.ID, 100, Signature{1, "d-aa"})
	if got != nil || first.Verdicts != "as verified" {
		t.Fatalf("the first request was noted: %v %q", got, first.Verdicts)
	}
	add(ep.ID, 100, Signature{1, "d-bb"})
	third, got := add(ep.ID, 100, Signature{2, "d-bb"}, Signature{1, "d-aa"})
	if fmt.Sprint(got) != "map[1:1 2:2]" {
		t.Fatalf("earlier %v, want check 1 → #1, check 2 → #2", got)
	}
	if stored, _ := s.Request(ctx, third.ID); stored.Verdicts != "noted map[1:1 2:2]" {
		t.Fatalf("stored verdicts %q", stored.Verdicts)
	}
	if _, got := add(other.ID, 100, Signature{1, "d-aa"}); got != nil {
		t.Fatalf("another endpoint's request counted: %v", got)
	}
	// Retention 1: the earlier request is pruned when this one is kept.
	if _, got := add(ep.ID, 1, Signature{1, "d-aa"}); got != nil {
		t.Fatalf("a pruned request counted: %v", got)
	}
	s.ClearRequests(ctx, ep.ID)
	if _, got := add(ep.ID, 100, Signature{1, "d-aa"}); got != nil {
		t.Fatalf("a cleared request counted: %v", got)
	}
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM signature_seen`).Scan(&n)
	if n != 2 { // the cleared register's last request and the other endpoint's
		t.Fatalf("%d signature rows, want 2", n)
	}
	// What is kept is the digest given, nothing else of the request.
	var digests []string
	rows, _ := s.db.QueryContext(ctx, `SELECT digest FROM signature_seen ORDER BY request_id`)
	for rows.Next() {
		var d string
		rows.Scan(&d)
		digests = append(digests, d)
	}
	rows.Close()
	if fmt.Sprint(digests) != "[d-aa d-aa]" {
		t.Fatalf("digests %v", digests)
	}
}

// TestSetVerdictsRecomputesRepeats: a re-check replaces a request's
// signatures and notes it against the earlier requests only.
func TestSetVerdictsRecomputesRepeats(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ep, _ := s.CreateEndpoint(ctx, "/re")
	var reqs []*Request
	for range 3 {
		r := sample(ep.ID, "{}")
		s.AddRequest(ctx, r, 100, nil)
		reqs = append(reqs, r)
	}
	var got map[int]int64
	// Oldest first, as a re-check goes: #1 has nothing before it.
	if err := s.SetVerdicts(ctx, reqs[0].ID, "pass", "v1", []Signature{{1, "d-x"}}, noted(&got)); err != nil || got != nil {
		t.Fatalf("%v %v", got, err)
	}
	s.SetVerdicts(ctx, reqs[1].ID, "pass", "v2", []Signature{{1, "d-y"}}, noted(&got))
	if err := s.SetVerdicts(ctx, reqs[2].ID, "pass", "v3", []Signature{{1, "d-x"}}, noted(&got)); err != nil || fmt.Sprint(got) != "map[1:1]" {
		t.Fatalf("%v %v", got, err)
	}
	// Re-checking #1 again does not see the later #3.
	got = nil
	s.SetVerdicts(ctx, reqs[0].ID, "pass", "v1", []Signature{{1, "d-x"}}, noted(&got))
	if got != nil {
		t.Fatalf("a later request counted: %v", got)
	}
	// Without signatures, the request's rows go.
	s.SetVerdicts(ctx, reqs[2].ID, "", "", nil, nil)
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM signature_seen WHERE request_id = ?`, reqs[2].ID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows left", n)
	}
}
