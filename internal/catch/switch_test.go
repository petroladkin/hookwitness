package catch

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

func turn(t *testing.T, st *store.Store, id int64, on bool) {
	t.Helper()
	if err := st.UpdateEndpoint(context.Background(), id, store.EndpointChange{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
}

// An off endpoint's path is answered as a path without an endpoint (404,
// kept in Unmatched, noted as the off endpoint's); turned on again, it
// catches with its behaviour and counter as they were.
func TestOffEndpointGoesToUnmatched(t *testing.T) {
	h, st, logs := setup(t, 1<<20)
	ctx := context.Background()
	ep := configure(t, st, "/orders", emulate.Config{Default: emulate.Reply{Status: 202},
		Rules: []emulate.Rule{{When: emulate.Condition{Kind: emulate.WhenFirst, N: 1}, Do: emulate.ActReply, Reply: emulate.Reply{Status: 503}}}})
	if got := send(h, "POST", "/orders", strings.NewReader("1")).Code; got != 503 {
		t.Fatalf("first request: %d, want 503 (rule 1)", got)
	}

	turn(t, st, ep, false)
	rec := send(h, "POST", "/orders?x=1", strings.NewReader("2"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("off: %d, want 404", rec.Code)
	}
	if e, _ := st.Endpoint(ctx, ep); e.Count != 1 || e.Counter != 1 {
		t.Fatalf("the off endpoint changed: %+v", e)
	}
	un, o := lastOutcome(t, st, store.Unmatched)
	if un.Path != "/orders" || un.Status != 404 || o.Source != emulate.FromOff || o.Status != 404 {
		t.Fatalf("Unmatched row: %+v, outcome %+v", un, o)
	}
	if !strings.Contains(o.Why(), "off") {
		t.Fatalf("why: %q", o.Why())
	}
	if !strings.Contains(logs.String(), "endpoint off") {
		t.Fatalf("not logged:\n%s", logs)
	}

	turn(t, st, ep, true)
	if got := send(h, "POST", "/orders", strings.NewReader("3")).Code; got != 202 {
		t.Fatalf("back on: %d, want 202 (default; rule 1 already used)", got)
	}
	_, o = lastOutcome(t, st, ep)
	if o.Counter != 2 || o.Source != emulate.FromDefault {
		t.Fatalf("back on: %+v, want counter 2 from the default", o)
	}
}

// Requests to an off endpoint are kept like any unmatched one: the body's
// head only.
func TestOffEndpointKeepsTheHead(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	ep, _ := st.CreateEndpoint(context.Background(), "/big")
	turn(t, st, ep.ID, false)
	send(h, "POST", "/big", strings.NewReader(strings.Repeat("x", unmatchedHead+10)))
	r, _ := lastOutcome(t, st, store.Unmatched)
	if r.Kept != unmatchedHead || r.Size != unmatchedHead+10 {
		t.Fatalf("kept %d of %d", r.Kept, r.Size)
	}
}

// A request held by a timeout rule when its endpoint is turned off finishes
// with its own outcome, on the endpoint.
func TestHeldRequestFinishesWhenTurnedOff(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	h.MaxWait = time.Hour
	holding := make(chan struct{})
	release := make(chan time.Time)
	h.after = func(time.Duration) <-chan time.Time { close(holding); return release }
	url := serve(t, h)
	ep := configure(t, st, "/hold", emulate.Config{Default: emulate.Reply{Status: 200},
		Rules: []emulate.Rule{{When: emulate.Condition{Kind: emulate.WhenEvery, N: 1}, Do: emulate.ActTimeout}}})
	done := make(chan struct{})
	go func() { do(t, http.DefaultClient, "POST", url+"/hold", ""); close(done) }()
	<-holding
	turn(t, st, ep, false)
	release <- time.Now()
	<-done
	h.Wait()
	_, o := lastOutcome(t, st, ep)
	if o.Action != emulate.ActTimeout {
		t.Fatalf("held request: %+v, want its timeout recorded on the endpoint", o)
	}
}
