package catch

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

// waitFor polls until cond holds; it fails the test after a long while.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited too long for %s", what)
		}
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
}

// A body that does not fit in MaxInFlight is answered 503 and recorded
// without it; once the bodies held are released, the next one fits.
func TestBusyWhenTooManyBodiesAreHeld(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	h.MaxInFlight = 100 << 10
	url := serve(t, h)
	ep := configure(t, st, "/up", emulate.Config{Default: emulate.Reply{Status: 200}})

	// A slow upload of unknown length holds 64 KiB while it arrives.
	pr, pw := io.Pipe()
	done := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Post(url+"/up", "application/octet-stream", pr)
		if err != nil {
			t.Error(err)
		}
		done <- resp
	}()
	pw.Write(bytes.Repeat([]byte("a"), 600))
	waitFor(t, "the upload to be held", func() bool { return h.heldNow() == 64<<10 })

	resp, _, err := do(t, http.DefaultClient, "POST", url+"/up", strings.Repeat("b", 60<<10))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("second upload: %d %v", resp.StatusCode, resp.Header)
	}
	last, o := lastOutcome(t, st, ep)
	req, err := st.Request(context.Background(), last.ID)
	if err != nil {
		t.Fatal(err)
	}
	if o.Source != emulate.FromBusy || o.Status != 503 || !req.TooLarge || len(req.Body) != 0 {
		t.Fatalf("recorded %+v, outcome %+v", req, o)
	}

	pw.Close()
	if r := <-done; r == nil || r.StatusCode != 200 {
		t.Fatalf("the slow upload: %v", r)
	}
	waitFor(t, "the bodies to be released", func() bool { return h.heldNow() == 0 })
	if resp, _, _ := do(t, http.DefaultClient, "POST", url+"/up", strings.Repeat("c", 60<<10)); resp.StatusCode != 200 {
		t.Fatalf("after release: %d", resp.StatusCode)
	}
}

// A request held by a timeout rule does not keep its body counted: holds
// last minutes and would starve everyone else.
func TestHoldReleasesTheBody(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	h.MaxInFlight = 100 << 10
	fire := make(chan time.Time)
	h.after = func(time.Duration) <-chan time.Time { return fire }
	url := serve(t, h)
	ep := configure(t, st, "/slow", emulate.Config{Default: emulate.Reply{Status: 200},
		Rules: []emulate.Rule{{When: emulate.Condition{Kind: emulate.WhenEvery, N: 1}, Do: emulate.ActTimeout}}})
	go do(t, http.DefaultClient, "POST", url+"/slow", strings.Repeat("a", 60<<10))
	waitFor(t, "the hold", func() bool {
		rs, _ := st.Requests(context.Background(), ep, 0, 1)
		return len(rs) == 1 && emulate.ParseOutcome(rs[0].Outcome, rs[0].Status).Action == emulate.ActTimeout
	})
	waitFor(t, "the body to be released during the hold", func() bool { return h.heldNow() == 0 })
	close(fire)
}

// The template data, which copies the body, is made before the store's
// transaction; the rules only set the number in it.
func TestTemplateDataBeforeTheTransaction(t *testing.T) {
	h, _, _ := setup(t, 1<<20)
	req := &store.Request{EndpointID: 1, Method: "POST", Path: "/p", Body: []byte("{}")}
	pl := h.plan(req, emulate.Config{Default: emulate.Reply{Status: 200, Body: "{{.N}}"}}.String(), false)
	data := pl.data
	if data == nil {
		t.Fatal("no template data before the transaction")
	}
	pl.decide(7)
	if pl.data != data || data.N != 7 {
		t.Fatalf("decide made new data or no number: %p %p %d", pl.data, data, data.N)
	}
}

// A path with no endpoint keeps the first 64 KiB of its body and records
// the whole size; over -max-body it is still not kept.
func TestUnmatchedKeepsTheHead(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	h.MaxInFlight = 1 << 20
	url := serve(t, h)
	for _, chunked := range []bool{false, true} {
		var body io.Reader = bytes.NewReader(bytes.Repeat([]byte("x"), 200<<10))
		if chunked {
			body = io.MultiReader(body) // no length: sent chunked
		}
		resp, err := http.Post(url+"/nowhere", "text/plain", body)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		last, _ := lastOutcome(t, st, store.Unmatched)
		req, err := st.Request(context.Background(), last.ID)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 404 || req.TooLarge || len(req.Body) != unmatchedHead || req.Size != 200<<10 {
			t.Fatalf("chunked %v: %d, kept %d of %d, too large %v", chunked, resp.StatusCode, len(req.Body), req.Size, req.TooLarge)
		}
	}
	resp, _ := http.Post(url+"/nowhere", "text/plain", io.MultiReader(bytes.NewReader(make([]byte, 2<<20))))
	resp.Body.Close()
	if req, _ := lastOutcome(t, st, store.Unmatched); !req.TooLarge || len(req.Body) != 0 {
		t.Fatalf("over max-body: kept %d, too large %v", len(req.Body), req.TooLarge)
	}
	waitFor(t, "nothing held", func() bool { return h.heldNow() == 0 })
}
