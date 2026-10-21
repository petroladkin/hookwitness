package catch

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/store"
)

// addBodies stores n requests of size-byte JSON bodies on ep.
func addBodies(t *testing.T, st *store.Store, ep int64, n, size int) []int64 {
	t.Helper()
	var ids []int64
	for i := range n {
		body := `{"type":"order.paid","order_id":` + strings.Repeat("1", 1+i%3) + `,"pad":"` + strings.Repeat("x", size) + `"}`
		r := &store.Request{EndpointID: ep, ReceivedAt: time.Now(), Method: "POST", Path: "/orders", Proto: "HTTP/1.1",
			Headers: []store.Header{{Name: "Content-Type", Value: "application/json"}}, Body: []byte(body), Size: int64(len(body)), Status: 202}
		if err := st.AddRequest(context.Background(), r, 1000, nil); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	return ids
}

// Re-check over bodies totalling more than a page's budget checks every
// request, the pages split by bytes.
func TestRecheckPagesByBytes(t *testing.T) {
	_, st, _ := setup(t, 1<<20)
	ep := configure(t, st, "/orders", schemaConfig(false))
	addBodies(t, st, ep, 12, 1000)
	old := store.PageBytes
	store.PageBytes = 2500 // about 2 bodies a page
	t.Cleanup(func() { store.PageBytes = old })
	e, _ := st.Endpoint(context.Background(), ep)
	n, err := Recheck(context.Background(), st, e, nil)
	if err != nil || n != 12 {
		t.Fatalf("re-checked %d: %v", n, err)
	}
	rs, _ := st.Requests(context.Background(), ep, 0, 20)
	for _, r := range rs {
		if r.Verdict != "pass" {
			t.Fatalf("request %d: verdict %q", r.No, r.Verdict)
		}
	}
}

// A draft past the bytes budget is refused, by id (the API) and as a set.
func TestDraftBytesBudget(t *testing.T) {
	_, st, _ := setup(t, 1<<20)
	ep := configure(t, st, "/orders", schemaConfig(false))
	ids := addBodies(t, st, ep, 5, 1000)
	old := MaxDraftBytes
	MaxDraftBytes = 3000
	t.Cleanup(func() { MaxDraftBytes = old })
	if DraftBytesMessage() != "at most 3000 bytes of bodies at a time" {
		t.Fatal(DraftBytesMessage())
	}
	if _, msg, err := DraftRequests(context.Background(), st, ep, ids); err != nil || msg != DraftBytesMessage() {
		t.Fatalf("by id: %q %v", msg, err)
	}
	reqs, msg, err := DraftRequests(context.Background(), st, ep, ids[:2])
	if err != nil || msg != "" || len(reqs) != 2 {
		t.Fatalf("within the budget: %q %v", msg, err)
	}
	all := make([]store.Request, 0, 5)
	for _, id := range ids {
		r, _ := st.Request(context.Background(), id)
		all = append(all, r)
	}
	if _, err := DraftSchema(schemaConfig(false), "/orders", all); err == nil || err.Error() != DraftBytesMessage() {
		t.Fatalf("a set over the budget: %v", err)
	}
	MaxDraftBytes = 64 << 20
	if DraftBytesMessage() != "at most 64 MiB of bodies at a time" {
		t.Fatal(DraftBytesMessage())
	}
}
