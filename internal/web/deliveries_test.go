package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/petroladkin/hookwitness/internal/store"
)

// A register page numbers its rows' deliveries with one query, and a live
// send with one more, giving the numbers Deliveries gives row by row.
func TestDeliveryNumbersOneQuery(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, _ := e.st.CreateEndpoint(ctx, "/retries")
	var added []*store.Request
	for i := range 120 {
		added = append(added, e.add(t, ep.ID, []string{`{"a":1}`, `{"b":2}`, `{"a":1}`, ``}[i%4], nil))
	}

	var mu sync.Mutex
	calls := 0
	got := map[int64]int{}
	old := deliveryNumbers
	deliveryNumbers = func(s *store.Store, ctx context.Context, reqs []store.Request) (map[int64]int, error) {
		n, err := old(s, ctx, reqs)
		mu.Lock()
		calls++
		for k, v := range n {
			got[k] = v
		}
		mu.Unlock()
		return n, err
	}
	t.Cleanup(func() { deliveryNumbers = old })

	if rec := e.do("GET", "/admin/e/"+id(ep.ID), nil); rec.Code != http.StatusOK {
		t.Fatalf("register: %d", rec.Code)
	}
	if calls != 1 || len(got) != 100 {
		t.Fatalf("a page of 100 rows: %d queries for %d rows, want 1 for 100", calls, len(got))
	}
	if got[added[118].ID] != 60 {
		t.Fatalf("the 60th delivery of {\"a\":1} numbered %d", got[added[118].ID])
	}
	for _, r := range added[20:] {
		want, _ := e.st.Deliveries(ctx, *r)
		if got[r.ID] != want {
			t.Fatalf("request %d: %d, Deliveries says %d", r.ID, got[r.ID], want)
		}
	}

	// The live feed sends the three missed rows with one more query.
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/admin/e/" + id(ep.ID) + "/events?after=" + id(added[116].ID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if evs := readEvents(t, resp.Body, 3); len(evs) != 3 {
		t.Fatalf("events %v", evs)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("%d queries after a live send of 3, want 2", calls)
	}
}
