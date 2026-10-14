package catch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

// serve runs the catcher on a real listener, so hijacking, resets and
// disconnects behave as they do in production.
func serve(t *testing.T, h *Handler) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.Catch(w, r, nil, false) }))
	t.Cleanup(srv.Close)
	return srv.URL
}

func configure(t *testing.T, st *store.Store, path string, c emulate.Config) int64 {
	t.Helper()
	if err := c.Validate(time.Hour); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	ep, err := st.CreateEndpoint(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetConfig(context.Background(), ep.ID, c.String()); err != nil {
		t.Fatal(err)
	}
	return ep.ID
}

func lastOutcome(t *testing.T, st *store.Store, ep int64) (store.Request, emulate.Outcome) {
	t.Helper()
	rs, err := st.Requests(context.Background(), ep, 0, 1)
	if err != nil || len(rs) == 0 {
		t.Fatalf("no request stored: %v", err)
	}
	return rs[0], emulate.ParseOutcome(rs[0].Outcome, rs[0].Status)
}

func do(t *testing.T, c *http.Client, method, url, body string, hdr ...string) (*http.Response, string, error) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b), nil
}

func TestDefaultReply(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	url := serve(t, h)
	ep := configure(t, st, "/slack", emulate.Config{
		Default: emulate.Reply{Status: 201, Headers: []emulate.Header{
			{Name: "X-Echo", Value: `{{.Header "X-Request-Id"}}`}, {Name: "Set-Cookie", Value: "a=1"}, {Name: "Set-Cookie", Value: "b=2"},
			{Name: "Server", Value: "real-client/1.0"},
		}, Body: `{"challenge":"{{.JSON "challenge"}}","n":{{.N}},"token":"{{.Var.TOKEN}}"}`},
		Vars: []emulate.Var{{Name: "TOKEN", Value: "tok_test"}},
	})
	resp, body, err := do(t, http.DefaultClient, "POST", url+"/slack", `{"challenge":"3eZb"}`, "X-Request-Id", "rq-1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 || body != `{"challenge":"3eZb","n":1,"token":"tok_test"}` {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Echo") != "rq-1" || strings.Join(resp.Header.Values("Set-Cookie"), ",") != "a=1,b=2" ||
		resp.Header.Get("Server") != "real-client/1.0" {
		t.Fatalf("headers %v", resp.Header)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		t.Fatalf("Content-Type %q sniffed in; only configured headers are sent", ct)
	}
	_, o := lastOutcome(t, st, ep)
	if o.Source != emulate.FromDefault || o.Status != 201 || o.Counter != 1 || o.Body != body || len(o.Headers) != 4 {
		t.Fatalf("outcome %+v", o)
	}

	// A change applies to the next request.
	st.SetConfig(context.Background(), ep, emulate.Config{Default: emulate.Reply{Status: 418}}.String())
	if resp, _, _ := do(t, http.DefaultClient, "POST", url+"/slack", ""); resp.StatusCode != 418 {
		t.Fatalf("after a change: %d", resp.StatusCode)
	}
}

// Real-time tests assert only what runner load cannot fake: a wait never
// ends early, and it ends well before the alternative (a delay applied twice,
// a hold past its cap or past the sender leaving). The exact durations asked
// for are checked through the timer seam in TestWaitsAskedFor.

// settled polls the newest request of ep until its outcome satisfies done:
// the handler records a hold only after the connection is closed, so the
// sender may see the end first.
func settled(t *testing.T, st *store.Store, ep int64, done func(emulate.Outcome) bool) (store.Request, emulate.Outcome) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rs, err := st.Requests(context.Background(), ep, 0, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(rs) > 0 {
			if o := emulate.ParseOutcome(rs[0].Outcome, rs[0].Status); done(o) || time.Now().After(deadline) {
				return rs[0], o
			}
		} else if time.Now().After(deadline) {
			t.Fatal("no request stored")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDelay(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	url := serve(t, h)
	ep := configure(t, st, "/slow", emulate.Config{Default: emulate.Reply{Status: 200, DelayMs: 300}})
	start := time.Now()
	if _, _, err := do(t, http.DefaultClient, "POST", url+"/slow", "x"); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	_, o := lastOutcome(t, st, ep)
	// Under 550 ms: the delay was applied once (twice would be 600).
	if took < 300*time.Millisecond || took >= 550*time.Millisecond {
		t.Fatalf("sender waited %v, want 300 ms (not early, not twice)", took)
	}
	if o.DelayMs < 300 || o.DelayMs >= 550 {
		t.Fatalf("recorded delay %d ms, want 300 (not early, not twice)", o.DelayMs)
	}
}

func TestTimeout(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	h.MaxWait = 2 * time.Second
	url := serve(t, h)
	ep := configure(t, st, "/hang", emulate.Config{Default: emulate.Reply{Status: 200},
		Rules: []emulate.Rule{{When: emulate.Condition{Kind: emulate.WhenEvery, N: 1}, Do: emulate.ActTimeout}}})

	// The sender gives up first: the hold ends then, long before the cap.
	impatient := &http.Client{Timeout: 150 * time.Millisecond}
	if _, _, err := do(t, impatient, "POST", url+"/hang", ""); err == nil {
		t.Fatal("a timeout must not answer")
	}
	r, o := settled(t, st, ep, func(o emulate.Outcome) bool { return o.WaitedMs > 0 })
	// The client's 150 ms include connecting, so the handler sees a bit less.
	if o.Action != emulate.ActTimeout || r.Status != 0 || o.WaitedMs < 100 || o.WaitedMs >= 1000 {
		t.Fatalf("sender gave up: status %d, outcome %+v, want a hold of about 150 ms, not the 2 s cap", r.Status, o)
	}

	// The cap passes first: closed without a response. The client's own
	// limit only keeps a broken cap from hanging the test.
	patient := &http.Client{Timeout: 10 * time.Second}
	h.MaxWait = 400 * time.Millisecond
	start := time.Now()
	_, _, err := do(t, patient, "POST", url+"/hang", "")
	if err == nil {
		t.Fatal("got a response after the hold")
	}
	if took := time.Since(start); took < 400*time.Millisecond || took >= 800*time.Millisecond {
		t.Fatalf("held %v, want the 400 ms cap (not early, not twice)", took)
	}
	if _, o := settled(t, st, ep, func(o emulate.Outcome) bool { return o.WaitedMs > 0 }); o.WaitedMs < 400 || o.WaitedMs >= 800 {
		t.Fatalf("waited %d ms, want the 400 ms cap", o.WaitedMs)
	}

	// Stop ends a hold at once, not at the cap of an hour.
	h.MaxWait = time.Hour
	go func() { time.Sleep(100 * time.Millisecond); h.Stop() }()
	start = time.Now()
	do(t, patient, "POST", url+"/hang", "")
	if took := time.Since(start); took >= 5*time.Second {
		t.Fatalf("Stop did not end the hold: %v", took)
	}
}

// asked replaces h's timer with one that fires at once and returns the
// durations asked for so far.
func asked(h *Handler) func() []time.Duration {
	var mu sync.Mutex
	var got []time.Duration
	h.after = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		got = append(got, d)
		mu.Unlock()
		c := make(chan time.Time, 1)
		c <- time.Now()
		return c
	}
	return func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Duration(nil), got...)
	}
}

// TestWaitsAskedFor: the exact durations the catcher waits for, without
// the clock: a delay, a jittered delay within its range, a delay and a
// timeout capped by the maximum wait.
func TestWaitsAskedFor(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	h.MaxWait = 400 * time.Millisecond
	waits := asked(h)
	url := serve(t, h)
	configure(t, st, "/slow", emulate.Config{Default: emulate.Reply{Status: 200, DelayMs: 300}})
	configure(t, st, "/jitter", emulate.Config{Default: emulate.Reply{Status: 200, DelayMs: 50, JitterMs: 100}})
	configure(t, st, "/long", emulate.Config{Default: emulate.Reply{Status: 200, DelayMs: 10_000}})
	configure(t, st, "/hang", emulate.Config{Default: emulate.Reply{Status: 200},
		Rules: []emulate.Rule{{When: emulate.Condition{Kind: emulate.WhenEvery, N: 1}, Do: emulate.ActTimeout}}})
	send := func(path string) {
		t.Helper()
		before := len(waits())
		do(t, http.DefaultClient, "POST", url+path, "")
		if n := len(waits()) - before; n != 1 {
			t.Fatalf("%s: %d waits, want one", path, n)
		}
	}
	last := func() time.Duration { w := waits(); return w[len(w)-1] }

	send("/slow")
	if d := last(); d != 300*time.Millisecond {
		t.Fatalf("delay asked %v, want 300 ms", d)
	}
	low, high := false, false
	for range 200 {
		send("/jitter")
		d := last()
		if d < 50*time.Millisecond || d > 150*time.Millisecond {
			t.Fatalf("jittered delay asked %v, want 50..150 ms", d)
		}
		low, high = low || d < 75*time.Millisecond, high || d > 125*time.Millisecond
	}
	if !low || !high {
		t.Fatalf("200 jittered delays do not spread over 50..150 ms (low %v, high %v)", low, high)
	}
	send("/long")
	if d := last(); d != 400*time.Millisecond {
		t.Fatalf("a 10 s delay asked %v, want the 400 ms cap", d)
	}
	send("/hang")
	if d := last(); d != 400*time.Millisecond {
		t.Fatalf("a timeout held for %v, want the 400 ms cap", d)
	}
}

func TestDropResetsTheConnection(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	url := serve(t, h)
	ep := configure(t, st, "/crash", emulate.Config{Default: emulate.Reply{Status: 200},
		Rules: []emulate.Rule{{When: emulate.Condition{Kind: emulate.WhenEvery, N: 1}, Do: emulate.ActDrop}}})
	_, _, err := do(t, http.DefaultClient, "POST", url+"/crash", `{"a":1}`)
	if err == nil {
		t.Fatal("a drop must not answer")
	}
	if !errors.Is(err, syscall.ECONNRESET) && !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("sender error %v, want a TCP reset (a crashed receiver)", err)
	}
	r, o := lastOutcome(t, st, ep)
	if o.Action != emulate.ActDrop || r.Status != 0 {
		t.Fatalf("outcome %+v", o)
	}
	full, _ := st.Request(context.Background(), r.ID)
	if string(full.Body) != `{"a":1}` {
		t.Fatalf("a dropped request is still kept: body %q", full.Body)
	}
}

func TestRulesInOrder(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	url := serve(t, h)
	ep := configure(t, st, "/flaky", emulate.Config{
		Default: emulate.Reply{Status: 200},
		Rules: []emulate.Rule{
			{When: emulate.Condition{Kind: emulate.WhenFirst, N: 2}, Do: emulate.ActReply, Reply: emulate.Reply{Status: 503, Body: "down"}},
			{When: emulate.Condition{Kind: emulate.WhenEvery, N: 3}, Do: emulate.ActReply, Reply: emulate.Reply{Status: 500}},
		},
	})
	var got []int
	for range 7 {
		resp, _, err := do(t, http.DefaultClient, "POST", url+"/flaky", "")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, resp.StatusCode)
	}
	if fmt.Sprint(got) != "[503 503 500 200 200 500 200]" {
		t.Fatalf("statuses %v", got)
	}
	_, o := lastOutcome(t, st, ep)
	if o.Source != emulate.FromDefault || o.Counter != 7 {
		t.Fatalf("last outcome %+v", o)
	}
	rs, _ := st.Requests(context.Background(), ep, 0, 10)
	sixth := emulate.ParseOutcome(rs[1].Outcome, rs[1].Status)
	if sixth.Source != emulate.FromRule || sixth.Rule != 2 || sixth.Label != "every 3rd" || sixth.Why() != "rule 2 · every 3rd" {
		t.Fatalf("6th outcome %+v", sixth)
	}
	// Reset: the first two fail again.
	st.ResetCounter(context.Background(), ep)
	if resp, _, _ := do(t, http.DefaultClient, "POST", url+"/flaky", ""); resp.StatusCode != 503 {
		t.Fatalf("after reset: %d", resp.StatusCode)
	}
}

func TestChecksRefuseAndDoNotCount(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	url := serve(t, h)
	ep := configure(t, st, "/guarded", emulate.Config{
		Default: emulate.Reply{Status: 200},
		Rules:   []emulate.Rule{{When: emulate.Condition{Kind: emulate.WhenFirst, N: 1}, Do: emulate.ActReply, Reply: emulate.Reply{Status: 503}}},
		Checks: []emulate.Check{
			{Header: "X-Env", Match: emulate.MatchEquals, Value: "test"},
			{Header: "Authorization", Match: emulate.MatchRegex, Value: `^Bearer `,
				Error: &emulate.Reply{Status: 401, Headers: []emulate.Header{{Name: "WWW-Authenticate", Value: "Bearer"}},
					Body: `{"error":"unauthorized","expected":"{{.Var.TOKEN}}"}`}},
		},
		Vars: []emulate.Var{{Name: "TOKEN", Value: "tok_test"}},
	})
	resp, body, _ := do(t, http.DefaultClient, "POST", url+"/guarded", "")
	if resp.StatusCode != 400 || !strings.Contains(body, "X-Env missing") {
		t.Fatalf("standard refusal: %d %q", resp.StatusCode, body)
	}
	resp, body, _ = do(t, http.DefaultClient, "POST", url+"/guarded", "", "X-Env", "test", "Authorization", "Basic x")
	if resp.StatusCode != 401 || body != `{"error":"unauthorized","expected":"tok_test"}` || resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("own refusal: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	r, o := lastOutcome(t, st, ep)
	if o.Action != emulate.ActRefused || o.Check != "check 2: header Authorization has a wrong value" || r.Status != 401 {
		t.Fatalf("refusal outcome %+v", o)
	}
	// Refused requests did not advance the counter: the first passing one is
	// still number 1 and meets the "first 1" rule.
	resp, _, _ = do(t, http.DefaultClient, "POST", url+"/guarded", "", "X-Env", "test", "Authorization", "Bearer tok_test")
	if resp.StatusCode != 503 {
		t.Fatalf("first passing request: %d, want 503 from the first-1 rule", resp.StatusCode)
	}
	if e, _ := st.Endpoint(context.Background(), ep); e.Counter != 1 {
		t.Fatalf("counter %d, want 1", e.Counter)
	}
}

func TestRenderErrorAnswers500(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	url := serve(t, h)
	ep := configure(t, st, "/broken", emulate.Config{Default: emulate.Reply{Status: 200, Body: `{{index .Body 1000}}`}})
	resp, body, _ := do(t, http.DefaultClient, "POST", url+"/broken", "short")
	if resp.StatusCode != 500 || !strings.Contains(body, "could not render") {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	r, o := lastOutcome(t, st, ep)
	if o.Action != emulate.ActError || !strings.Contains(o.Error, "index out of range") || r.Status != 500 {
		t.Fatalf("status %d outcome %+v", r.Status, o)
	}
}

// TestEveryThirdIsExact is AC-9: 99 concurrent requests, an every-3rd rule,
// exactly 33 matches.
func TestEveryThirdIsExact(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	url := serve(t, h)
	ep := configure(t, st, "/third", emulate.Config{Default: emulate.Reply{Status: 200},
		Rules: []emulate.Rule{{When: emulate.Condition{Kind: emulate.WhenEvery, N: 3}, Do: emulate.ActReply, Reply: emulate.Reply{Status: 500}}}})
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	for range 99 {
		wg.Go(func() {
			resp, _, err := do(t, http.DefaultClient, "POST", url+"/third", "")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			codes[resp.StatusCode]++
			mu.Unlock()
		})
	}
	wg.Wait()
	rs, _ := st.Requests(context.Background(), ep, 0, 1000)
	matched := 0
	for _, r := range rs {
		if emulate.ParseOutcome(r.Outcome, r.Status).Source == emulate.FromRule {
			matched++
		}
	}
	if codes[500] != 33 || codes[200] != 66 || matched != 33 {
		t.Fatalf("answers %v, stored rule matches %d; want 33 x 500, 66 x 200, 33", codes, matched)
	}
}

// TestWaitRecordsTheHoldBeforeShutdown: Stop ends a hold and Wait returns
// only once its outcome (with the wait) is stored, so the store may close.
func TestWaitRecordsTheHoldBeforeShutdown(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	h.MaxWait = time.Hour
	url := serve(t, h)
	ep := configure(t, st, "/hold", emulate.Config{Default: emulate.Reply{Status: 200},
		Rules: []emulate.Rule{{When: emulate.Condition{Kind: emulate.WhenEvery, N: 1}, Do: emulate.ActTimeout}}})
	go do(t, http.DefaultClient, "POST", url+"/hold", "")
	time.Sleep(200 * time.Millisecond)
	h.Stop()
	h.Wait()
	_, o := lastOutcome(t, st, ep)
	if o.Action != emulate.ActTimeout || o.WaitedMs < 150 {
		t.Fatalf("after Wait: %+v, want the hold's wait recorded", o)
	}
}

// TestSenderLeftDuringDelay: a reply the sender gave up on before its delay
// ran out was never delivered, so it is not recorded as one.
func TestSenderLeftDuringDelay(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	url := serve(t, h)
	ep := configure(t, st, "/slow", emulate.Config{Default: emulate.Reply{Status: 200, DelayMs: 800}})
	impatient := &http.Client{Timeout: 200 * time.Millisecond}
	if _, _, err := do(t, impatient, "POST", url+"/slow", "x"); err == nil {
		t.Fatal("the sender got an answer before its timeout")
	}
	var r store.Request
	var o emulate.Outcome
	for range 50 { // the handler records it once it notices the sender left
		if r, o = lastOutcome(t, st, ep); o.Action == emulate.ActAbandoned {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if o.Action != emulate.ActAbandoned || r.Status != 0 || o.Status != 0 || o.Would != 200 || o.DelayMs != 800 ||
		o.WaitedMs < 100 || o.WaitedMs >= 800 {
		t.Fatalf("status %d, outcome %+v", r.Status, o)
	}
	if o.Good() || !o.HeldUp() || o.Stamp() != "sender left" {
		t.Fatalf("classified %v %v %q", o.Good(), o.HeldUp(), o.Stamp())
	}

	// A delayed refusal the sender does not wait for is the same.
	refuse := configure(t, st, "/strict", emulate.Config{Default: emulate.Reply{Status: 200},
		Checks: []emulate.Check{{Kind: emulate.KindHeader, Header: "X-Key", Match: emulate.MatchPresent,
			Error: &emulate.Reply{Status: 403, DelayMs: 800}}}})
	do(t, impatient, "POST", url+"/strict", "x")
	for range 50 {
		if _, o = lastOutcome(t, st, refuse); o.Action == emulate.ActAbandoned {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if o.Action != emulate.ActAbandoned || o.Would != 403 || o.Source != emulate.FromCheck {
		t.Fatalf("refusal: %+v", o)
	}

	// A sender that waits the delay out gets its reply, recorded as one.
	patient := configure(t, st, "/patient", emulate.Config{Default: emulate.Reply{Status: 201, DelayMs: 100}})
	if resp, _, err := do(t, http.DefaultClient, "POST", url+"/patient", "x"); err != nil || resp.StatusCode != 201 {
		t.Fatalf("patient: %v %v", resp, err)
	}
	if r, o := lastOutcome(t, st, patient); o.Action != emulate.ActReply || r.Status != 201 || o.Would != 0 {
		t.Fatalf("patient: %d %+v", r.Status, o)
	}
}

// TestRawReplyBody: a reply given as body_base64 sends exactly those bytes,
// and the outcome keeps a body that is not UTF-8 as base64.
func TestRawReplyBody(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	url := serve(t, h)
	ep := configure(t, st, "/odd", emulate.Config{Default: emulate.Reply{Status: 200, BodyBase64: "//57e319"}}) // ff fe { { } }
	resp, body, err := do(t, http.DefaultClient, "POST", url+"/odd", "x")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || body != "\xff\xfe{{}}" || resp.Header.Get("Content-Length") != "6" {
		t.Fatalf("%d %q %v", resp.StatusCode, body, resp.Header)
	}
	if _, o := lastOutcome(t, st, ep); o.BodyBase64 != "//57e319" || o.Body != "" {
		t.Fatalf("outcome %+v", o)
	}
}
