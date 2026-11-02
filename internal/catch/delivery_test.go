package catch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// stored catches a request the way the handler stores it: checks run
// before the transaction, the delivery checks finished inside it.
func stored(t *testing.T, st *store.Store, ep int64, cfg emulate.Config, at time.Time, status int, body string, hs ...store.Header) *store.Request {
	t.Helper()
	r := &store.Request{EndpointID: ep, ReceivedAt: at, Method: "POST", Path: "/d", Proto: "HTTP/1.1", Headers: hs,
		Body: []byte(body), Size: int64(len(body)), Status: status,
		Outcome: emulate.Outcome{Action: emulate.ActReply, Status: status}.String()}
	er := toEmulate(*r)
	vs, _ := cfg.Verify(er, at, true, nil)
	r.Verdict, r.Verdicts = emulate.Overall(vs), emulate.VerdictsJSON(vs)
	r.Delivery = delivery(cfg, er, vs, at)
	if err := st.AddRequest(context.Background(), r, store.Keep{Normal: 1000, Anomalies: 1000}, nil); err != nil {
		t.Fatal(err)
	}
	return r
}

// verdictOf reads a stored request's verdict of check n.
func verdictOf(t *testing.T, st *store.Store, id int64, n int) auth.Verdict {
	t.Helper()
	r, err := st.Request(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var vs []auth.Verdict
	json.Unmarshal([]byte(r.Verdicts), &vs)
	for _, v := range vs {
		if v.Check == n {
			return v
		}
	}
	t.Fatalf("request %d has no verdict of check %d: %s", id, n, r.Verdicts)
	return auth.Verdict{}
}

func openDeliveryStore(t *testing.T, cfg emulate.Config) (*store.Store, int64) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ep, _ := st.CreateEndpoint(context.Background(), "/d")
	st.SetConfig(context.Background(), ep.ID, cfg.String())
	return st, ep.ID
}

var githubCfg = emulate.Config{Default: emulate.Reply{Status: 200}, Event: &emulate.Source{From: emulate.FromHeader, Key: "X-GitHub-Delivery"},
	Checks: []emulate.Check{{Kind: emulate.KindDuplicates}, {Kind: emulate.KindBackoff, FirstS: 1}}}

// A GitHub-style sender: answered 503, retried after 1 s and answered 200,
// then delivered again: the duplicate fails, naming the 200.
func TestDuplicatesAndBackoffThroughTheStore(t *testing.T) {
	st, ep := openDeliveryStore(t, githubCfg)
	id := func(v string) store.Header { return store.Header{Name: "X-GitHub-Delivery", Value: v} }
	r1 := stored(t, st, ep, githubCfg, t0, 503, `{}`, id("d-1"))
	r2 := stored(t, st, ep, githubCfg, t0.Add(1000*time.Millisecond), 200, `{}`, id("d-1"))
	r3 := stored(t, st, ep, githubCfg, t0.Add(3000*time.Millisecond), 200, `{}`, id("d-1"))
	r4 := stored(t, st, ep, githubCfg, t0.Add(3500*time.Millisecond), 200, `{}`, id("d-2"))
	r5 := stored(t, st, ep, githubCfg, t0.Add(3600*time.Millisecond), 200, `{}`)
	for _, c := range []struct {
		r      *store.Request
		check  int
		result auth.Result
		reason string
	}{
		{r1, 1, auth.Pass, "first delivery of d-1"},
		{r1, 2, auth.NotChecked, "first attempt"},
		{r2, 1, auth.Pass, "attempt 2 of d-1; #1 was answered 503"},
		{r2, 2, auth.Pass, "attempt 2 after 1 s, expected 1 s"},
		{r3, 1, auth.Fail, "duplicate of #2, which was answered 200"},
		{r3, 2, auth.Pass, "attempt 3 after 2 s, expected 2 s"},
		{r4, 1, auth.Pass, "first delivery of d-2"},
		{r5, 1, auth.NotVerifiable, "header X-GitHub-Delivery missing"},
	} {
		v := verdictOf(t, st, c.r.ID, c.check)
		if v.Result != c.result || !strings.Contains(v.Reason, c.reason) {
			t.Errorf("#%d check %d: %s %q, want %s %q", c.r.No, c.check, v.Result, v.Reason, c.result, c.reason)
		}
	}
	got, _ := st.Request(context.Background(), r3.ID)
	if got.Verdict != "fail" || !got.Anomaly || got.Event == nil || *got.Event != "d-1" || got.Attempt != 3 {
		t.Fatalf("r3 stored: verdict %s anomaly %v event %v attempt %d", got.Verdict, got.Anomaly, got.Event, got.Attempt)
	}
	ev := "d-1"
	list, _ := st.List(context.Background(), ep, store.ListQuery{Event: &ev})
	if len(list) != 3 {
		t.Fatalf("event=d-1 lists %d requests", len(list))
	}
}

// Order by a sequence, per group: a lower value than another event's is
// out of order; a retry of an earlier event is not.
func TestOrderThroughTheStore(t *testing.T) {
	cfg := emulate.Config{Default: emulate.Reply{Status: 200}, Event: &emulate.Source{From: emulate.FromBody, Key: "/id"},
		Checks: []emulate.Check{{Kind: emulate.KindOrder, Order: &emulate.OrderSpec{Source: emulate.Source{From: emulate.FromBody, Key: "/seq"}, As: "number"},
			Group: &emulate.Source{From: emulate.FromBody, Key: "/obj"}}}}
	st, ep := openDeliveryStore(t, cfg)
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Second) }
	a := stored(t, st, ep, cfg, at(0), 200, `{"id":"evt_1","seq":40,"obj":"cus_1"}`)
	b := stored(t, st, ep, cfg, at(1), 200, `{"id":"evt_2","seq":41,"obj":"cus_1"}`)
	retry := stored(t, st, ep, cfg, at(2), 200, `{"id":"evt_1","seq":40,"obj":"cus_1"}`)
	late := stored(t, st, ep, cfg, at(3), 200, `{"id":"evt_3","seq":39,"obj":"cus_1"}`)
	other := stored(t, st, ep, cfg, at(4), 200, `{"id":"evt_4","seq":1,"obj":"cus_2"}`)
	none := stored(t, st, ep, cfg, at(5), 200, `{"id":"evt_5","obj":"cus_1"}`)
	for _, c := range []struct {
		r      *store.Request
		result auth.Result
		reason string
	}{
		{a, auth.Pass, "first in order"},
		{b, auth.Pass, "41 after 40 (#1)"},
		{retry, auth.Pass, "attempt 2 of evt_1: judged on its first delivery"},
		{late, auth.Fail, "out of order: 39 after 41 (#2)"},
		{other, auth.Pass, "first in order"},
		{none, auth.NotVerifiable, "no value at /seq"},
	} {
		v := verdictOf(t, st, c.r.ID, 1)
		if v.Result != c.result || !strings.Contains(v.Reason, c.reason) {
			t.Errorf("#%d: %s %q, want %s %q", c.r.No, v.Result, v.Reason, c.result, c.reason)
		}
	}
}

// Concurrent attempts of one event get the numbers 1…n, no gap or repeat.
func TestAttemptsUnderConcurrency(t *testing.T) {
	st, ep := openDeliveryStore(t, githubCfg)
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Go(func() {
			stored(t, st, ep, githubCfg, t0.Add(time.Duration(i)*time.Second), 503, fmt.Sprint(i), store.Header{Name: "X-GitHub-Delivery", Value: "d-9"})
		})
	}
	wg.Wait()
	reqs, _ := st.Requests(context.Background(), ep, 0, 100)
	seen := map[int]bool{}
	for _, r := range reqs {
		if r.Attempt < 1 || r.Attempt > 30 || seen[r.Attempt] {
			t.Fatalf("attempt %d (seen %v)", r.Attempt, seen)
		}
		seen[r.Attempt] = true
	}
	if len(seen) != 30 {
		t.Fatalf("%d attempts", len(seen))
	}
}

// Re-check after a change of the event source gives what a fresh store fed
// the same requests gives.
func TestRecheckRegroups(t *testing.T) {
	byBody := emulate.Config{Default: emulate.Reply{Status: 200}, Checks: []emulate.Check{{Kind: emulate.KindDuplicates}}}
	st, ep := openDeliveryStore(t, byBody)
	bodies := []string{`{"id":"e1","n":1}`, `{"id":"e1","n":2}`, `{"id":"e2","n":1}`, `{"id":"e1","n":3}`}
	for i, b := range bodies {
		stored(t, st, ep, byBody, t0.Add(time.Duration(i)*time.Second), 200, b)
	}
	byID := byBody
	byID.Event = &emulate.Source{From: emulate.FromBody, Key: "/id"}
	st.SetConfig(context.Background(), ep, byID.String())
	e, _ := st.Endpoint(context.Background(), ep)
	if _, err := Recheck(context.Background(), st, e, nil); err != nil {
		t.Fatal(err)
	}
	fresh, fep := openDeliveryStore(t, byID)
	for i, b := range bodies {
		stored(t, fresh, fep, byID, t0.Add(time.Duration(i)*time.Second), 200, b)
	}
	a, _ := st.Requests(context.Background(), ep, 0, 10)
	b, _ := fresh.Requests(context.Background(), fep, 0, 10)
	for i := range a {
		va, vb := verdictOf(t, st, a[i].ID, 1), verdictOf(t, fresh, b[i].ID, 1)
		if va.Result != vb.Result || va.Reason != vb.Reason || a[i].Attempt != b[i].Attempt || *a[i].Event != *b[i].Event {
			t.Errorf("#%d: re-checked %s %q attempt %d, fresh %s %q attempt %d", a[i].No, va.Result, va.Reason, a[i].Attempt, vb.Result, vb.Reason, b[i].Attempt)
		}
	}
	if v := verdictOf(t, st, a[0].ID, 1); v.Result != auth.Fail { // e1's third, after 200s
		t.Errorf("newest e1 after a re-check: %s %q", v.Result, v.Reason)
	}
}

// Through the handler: a GitHub-style sender is answered 503 by a rule, then
// 429 with Retry-After: 10, retries 3 s later (too early) and 10 s after
// that, and then delivers the event again after its 200 (a duplicate).
func TestDeliveryThroughTheHandler(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	cfg := emulate.Config{Default: emulate.Reply{Status: 200}, Event: &emulate.Source{From: emulate.FromHeader, Key: "X-GitHub-Delivery"},
		Rules: []emulate.Rule{
			{When: emulate.Condition{Kind: emulate.WhenFirst, N: 1}, Do: emulate.ActReply, Reply: emulate.Reply{Status: 503}},
			{When: emulate.Condition{Kind: emulate.WhenFirst, N: 3}, Do: emulate.ActReply,
				Reply: emulate.Reply{Status: 429, Headers: []emulate.Header{{Name: "Retry-After", Value: "10"}}}},
		},
		Checks: []emulate.Check{{Kind: emulate.KindDuplicates}, {Kind: emulate.KindBackoff, FirstS: 1, Factor: 2}}}
	ep := configure(t, st, "/github/push", cfg)
	var clock time.Time
	h.now = func() time.Time { return clock }
	deliver := func(at float64) *store.Request {
		clock = t0.Add(time.Duration(at * float64(time.Second)))
		r := httptest.NewRequest("POST", "/github/push", strings.NewReader(`{"ref":"main"}`))
		r.Header.Set("X-GitHub-Delivery", "d-1")
		h.Catch(httptest.NewRecorder(), r, nil, false)
		got, _ := st.Requests(context.Background(), ep, 0, 1)
		return &got[0]
	}
	type want struct {
		status          int
		dup, backoff    auth.Result
		dupWhy, backWhy string
	}
	for i, c := range []struct {
		at float64
		w  want
	}{
		{0, want{503, auth.Pass, auth.NotChecked, "first delivery of d-1", "first attempt"}},
		{1, want{429, auth.Pass, auth.Pass, "#1 was answered 503", "attempt 2 after 1 s, expected 1 s"}},
		{4, want{429, auth.Pass, auth.Fail, "#2 was answered 429", "attempt 3 after 3 s; #2 asked Retry-After: 10 s"}},
		{14, want{200, auth.Pass, auth.Pass, "#3 was answered 429", "expected 10 s"}},
		{22, want{200, auth.Fail, auth.Pass, "duplicate of #4, which was answered 200", "attempt 5 after 8 s, expected 8 s"}},
	} {
		r := deliver(c.at)
		dup, back := verdictOf(t, st, r.ID, 1), verdictOf(t, st, r.ID, 2)
		if r.Status != c.w.status || r.Attempt != i+1 ||
			dup.Result != c.w.dup || !strings.Contains(dup.Reason, c.w.dupWhy) ||
			back.Result != c.w.backoff || !strings.Contains(back.Reason, c.w.backWhy) {
			t.Errorf("attempt %d: status %d attempt %d, duplicates %s %q, backoff %s %q; want %+v",
				i+1, r.Status, r.Attempt, dup.Result, dup.Reason, back.Result, back.Reason, c.w)
		}
	}
}
