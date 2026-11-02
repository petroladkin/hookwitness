package catch

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

// clocked gives h a clock the test moves: deliver(s) sends at t0 + s.
func clocked(h *Handler) func(s float64) {
	var mu sync.Mutex
	var clock time.Time
	h.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	return func(s float64) {
		mu.Lock()
		clock = t0.Add(time.Duration(s * float64(time.Second)))
		mu.Unlock()
	}
}

// TestClockRulesThroughTheHandler: a window, a flap by time and one by
// count, a rate block and its recovery, the template variables, and the
// outcome's time since the start.
func TestClockRulesThroughTheHandler(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	ra := emulate.Reply{Status: 429, Headers: []emulate.Header{{Name: "Retry-After", Value: "{{.RetryAfter}}"}},
		Body: "{{.Elapsed}} {{.Attempt}} {{.N}}"}
	for _, c := range []struct {
		name string
		when emulate.Condition
		at   []float64
		want []string // status, Retry-After, body
	}{
		{"window", emulate.Condition{Kind: emulate.WhenWindow, ToS: 2}, []float64{10, 10.5, 11.9, 12, 40},
			[]string{"429 2 0 1 1", "429 2 0.5 1 2", "429 1 1.9 1 3", "200", "200"}},
		{"gone after", emulate.Condition{Kind: emulate.WhenWindow, FromS: 1}, []float64{0, 0.999, 1, 100},
			[]string{"200", "200", "429 0 1 1 3", "429 0 100 1 4"}},
		{"flap by time", emulate.Condition{Kind: emulate.WhenFlap, Down: 1, Up: 1, Unit: "s"}, []float64{0, 0.25, 1, 1.75, 2.5},
			[]string{"429 1 0 1 1", "429 1 0.25 1 2", "200", "200", "429 1 2.5 1 5"}},
		{"flap by count", emulate.Condition{Kind: emulate.WhenFlap, Down: 1, Up: 2, Unit: "requests"}, []float64{0, 0, 0, 0},
			[]string{"429 0 0 1 1", "200", "200", "429 0 0 1 4"}},
		{"rate", emulate.Condition{Kind: emulate.WhenRate, N: 2, PerS: 1, BlockS: 2}, []float64{0, 0.1, 0.2, 1.5, 2.2, 2.3},
			[]string{"200", "200", "429 2 0.2 1 3", "429 1 1.5 1 4", "200", "200"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := "/" + strings.ReplaceAll(c.name, " ", "-")
			ep := configure(t, st, path, emulate.Config{Default: emulate.Reply{Status: 200},
				Rules: []emulate.Rule{{When: c.when, Do: emulate.ActReply, Reply: ra}}})
			move := clocked(h)
			for i, s := range c.at {
				move(s)
				rec := send(h, "POST", path, strings.NewReader("{}"))
				got := rec.Result().Status[:3]
				if rec.Code == 429 {
					got += " " + rec.Header().Get("Retry-After") + " " + rec.Body.String()
				}
				if got != c.want[i] {
					t.Errorf("at %g s: %q, want %q", s, got, c.want[i])
				}
			}
			r, o := lastOutcome(t, st, ep)
			e, _ := st.Endpoint(context.Background(), ep)
			if !e.StartedAt.Equal(t0.Add(time.Duration(c.at[0] * float64(time.Second)))) {
				t.Errorf("started at %v", e.StartedAt)
			}
			if r.Status == 429 && (o.ElapsedMs == nil || o.Label != c.when.Label()) {
				t.Errorf("outcome %s", r.Outcome)
			}
		})
	}
}

// TestResetStartsTheClockAgain: the reset clears the counter, the start and
// a rate block; turning the endpoint off and on keeps them.
func TestResetStartsTheClockAgain(t *testing.T) {
	ctx := context.Background()
	h, st, _ := setup(t, 1<<20)
	ep := configure(t, st, "/r", emulate.Config{Default: emulate.Reply{Status: 200}, Rules: []emulate.Rule{
		{When: emulate.Condition{Kind: emulate.WhenRate, N: 1, PerS: 60, BlockS: 60}, Do: emulate.ActReply, Reply: emulate.Reply{Status: 429}}}})
	move := clocked(h)
	move(5)
	send(h, "POST", "/r", nil)
	move(6)
	if rec := send(h, "POST", "/r", nil); rec.Code != 429 {
		t.Fatalf("second within a minute: %d", rec.Code)
	}
	off, on := false, true
	st.UpdateEndpoint(ctx, ep, store.EndpointChange{Enabled: &off})
	st.UpdateEndpoint(ctx, ep, store.EndpointChange{Enabled: &on})
	move(7)
	if rec := send(h, "POST", "/r", nil); rec.Code != 429 {
		t.Fatalf("off and on: %d, want the block kept", rec.Code)
	}
	if e, _ := st.Endpoint(ctx, ep); e.Counter != 3 || !e.StartedAt.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("before the reset: counter %d, started %v", e.Counter, e.StartedAt)
	}
	if err := st.ResetCounter(ctx, ep); err != nil {
		t.Fatal(err)
	}
	if e, _ := st.Endpoint(ctx, ep); e.Counter != 0 || !e.StartedAt.IsZero() {
		t.Fatalf("after the reset: counter %d, started %v", e.Counter, e.StartedAt)
	}
	move(8)
	if rec := send(h, "POST", "/r", nil); rec.Code != 200 {
		t.Fatalf("after the reset: %d, want the block gone", rec.Code)
	}
	if e, _ := st.Endpoint(ctx, ep); e.Counter != 1 || !e.StartedAt.Equal(t0.Add(8*time.Second)) {
		t.Fatalf("after the reset and a request: counter %d, started %v", e.Counter, e.StartedAt)
	}
}

// TestRateIsExactUnderConcurrency is AC-3: 50 requests at once against
// "over 5 in 60 s" let exactly 5 through to the default reply.
func TestRateIsExactUnderConcurrency(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	ep := configure(t, st, "/rate", emulate.Config{Default: emulate.Reply{Status: 200}, Rules: []emulate.Rule{
		{When: emulate.Condition{Kind: emulate.WhenRate, N: 5, PerS: 60, BlockS: 60}, Do: emulate.ActReply, Reply: emulate.Reply{Status: 429}}}})
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { send(h, "POST", "/rate", strings.NewReader("x")) })
	}
	wg.Wait()
	rs, _ := st.Requests(context.Background(), ep, 0, 100)
	through := 0
	for _, r := range rs {
		if r.Status == 200 {
			through++
		}
	}
	if len(rs) != 50 || through != 5 {
		t.Fatalf("%d stored, %d through; want 50 and 5", len(rs), through)
	}
}

// TestAttemptsOfEachEvent: "first attempt of each event" by a GitHub-style
// event id, without a delivery check; and by the body without an event
// source.
func TestAttemptsOfEachEvent(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	rule := []emulate.Rule{{When: emulate.Condition{Kind: emulate.WhenAttempts, N: 1}, Do: emulate.ActReply,
		Reply: emulate.Reply{Status: 503, Body: "attempt {{.Attempt}}"}}}
	configure(t, st, "/byid", emulate.Config{Default: emulate.Reply{Status: 200, Body: "attempt {{.Attempt}}"}, Rules: rule,
		Event: &emulate.Source{From: emulate.FromHeader, Key: "X-GitHub-Delivery"}})
	configure(t, st, "/bybody", emulate.Config{Default: emulate.Reply{Status: 200, Body: "attempt {{.Attempt}}"}, Rules: rule})
	deliver := func(path, id, body string) string {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		if id != "" {
			r.Header.Set("X-GitHub-Delivery", id)
		}
		rec := httptest.NewRecorder()
		h.Catch(rec, r, nil, false)
		return rec.Result().Status[:3] + " " + rec.Body.String()
	}
	for i, c := range []struct{ path, id, body, want string }{
		{"/byid", "d-1", "{}", "503 attempt 1"},
		{"/byid", "d-1", "{}", "200 attempt 2"},
		{"/byid", "d-2", "{}", "503 attempt 1"},
		{"/byid", "d-1", "{}", "200 attempt 3"},
		{"/byid", "", "{}", "503 attempt 1"}, // no event id: a first attempt
		{"/bybody", "", `{"a":1}`, "503 attempt 1"},
		{"/bybody", "", `{"a":1}`, "200 attempt 2"},
		{"/bybody", "", `{"a":2}`, "503 attempt 1"},
	} {
		if got := deliver(c.path, c.id, c.body); got != c.want {
			t.Errorf("%d %s %s: %q, want %q", i+1, c.path, c.id, got, c.want)
		}
	}
}

// TestRetryAfterVerdict is AC-5: a rule answers 429 with Retry-After: 2; a
// retry 1 s later fails the backoff check, naming the request that asked,
// and one 2 s later passes.
func TestRetryAfterVerdict(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	ep := configure(t, st, "/ra", emulate.Config{Default: emulate.Reply{Status: 200},
		Event: &emulate.Source{From: emulate.FromHeader, Key: "X-Event"},
		Rules: []emulate.Rule{{When: emulate.Condition{Kind: emulate.WhenAttempts, N: 1}, Do: emulate.ActReply,
			Reply: emulate.Reply{Status: 429, Headers: []emulate.Header{{Name: "Retry-After", Value: "2"}}}}},
		Checks: []emulate.Check{{Kind: emulate.KindBackoff}}})
	move := clocked(h)
	deliver := func(s float64, event string) store.Request {
		move(s)
		r := httptest.NewRequest("POST", "/ra", strings.NewReader("{}"))
		r.Header.Set("X-Event", event)
		h.Catch(httptest.NewRecorder(), r, nil, false)
		got, _ := st.Requests(context.Background(), ep, 0, 1)
		return got[0]
	}
	for _, c := range []struct {
		at     float64
		event  string
		status int
		result auth.Result
		why    string
	}{
		{0, "e-1", 429, auth.NotChecked, "first attempt"},
		{1, "e-1", 200, auth.Fail, "attempt 2 after 1 s; #1 asked Retry-After: 2 s"},
		{10, "e-2", 429, auth.NotChecked, "first attempt"},
		{12, "e-2", 200, auth.Pass, "attempt 2 after 2 s; #3 asked Retry-After: 2 s"},
	} {
		r := deliver(c.at, c.event)
		v := verdictOf(t, st, r.ID, 1)
		if r.Status != c.status || v.Result != c.result || !strings.Contains(v.Reason, c.why) {
			t.Errorf("at %g s (%s): %d, %s %q; want %d, %s %q", c.at, c.event, r.Status, v.Result, v.Reason, c.status, c.result, c.why)
		}
	}
}
