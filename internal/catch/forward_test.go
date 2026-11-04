package catch

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/forward"
	wsend "github.com/petroladkin/hookwitness/internal/send"
	"github.com/petroladkin/hookwitness/internal/store"
	"github.com/petroladkin/hookwitness/internal/wire"
)

// fakeReceiver is a raw TCP receiver: it captures each request's bytes and
// answers with answer (called per request with its number from 1).
type fakeReceiver struct {
	ln    net.Listener
	mu    sync.Mutex
	got   []string
	count atomic.Int32
}

func newFakeReceiver(t *testing.T, answer func(c net.Conn, n int32)) *fakeReceiver {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fr := &fakeReceiver{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				// The bytes as they came, read as far as the request goes.
				var raw bytes.Buffer
				br := bufio.NewReader(io.TeeReader(c, &raw))
				if req, err := http.ReadRequest(br); err == nil {
					io.Copy(io.Discard, req.Body)
				}
				fr.mu.Lock()
				fr.got = append(fr.got, raw.String())
				fr.mu.Unlock()
				answer(c, fr.count.Add(1))
			}()
		}
	}()
	return fr
}

func (fr *fakeReceiver) url() string { return "http://" + fr.ln.Addr().String() }

func (fr *fakeReceiver) last() string {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.got) == 0 {
		return ""
	}
	return fr.got[len(fr.got)-1]
}

func answering(s string) func(net.Conn, int32) {
	return func(c net.Conn, _ int32) { io.WriteString(c, s) }
}

// final waits for a request's outcome to stop being pending: forwards end
// after the answer is written.
func final(t *testing.T, st *store.Store, ep int64, n int) []store.Request {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rs, _ := st.Requests(context.Background(), ep, 0, 100)
		pending := len(rs) < n
		for _, r := range rs {
			if strings.Contains(r.Outcome, `"pending":true`) {
				pending = true
			}
		}
		if !pending {
			return rs
		}
		if time.Now().After(deadline) {
			t.Fatalf("still pending after 5 s: %+v", rs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestForwardThroughTheCatcher is AC-1, AC-2 and AC-3: a signed request
// reaches the receiver at its path plus the received query, body and
// headers as received, Host the target's; the sender gets the receiver's
// status, headers and body (gzip untouched, a 302 not followed), protected
// on the catch origin; both sides are recorded.
func TestForwardThroughTheCatcher(t *testing.T) {
	gz := "\x1f\x8b\x08\x00\x00\x00\x00\x00\x00\xff"
	fr := newFakeReceiver(t, answering("HTTP/1.1 302 Found\r\nLocation: /elsewhere\r\nSet-Cookie: "+access.CookieName+"=stolen\r\n"+
		"Set-Cookie: "+access.FlowCookieName+"=planted; Path=/admin/\r\nSet-Cookie: app=1\r\nContent-Security-Policy: default-src 'self'\r\nContent-Encoding: gzip\r\nConnection: close\r\n"+
		"Content-Length: 10\r\n\r\n"+gz))
	h, st, _ := setup(t, 1<<20)
	url := serve(t, h)
	ep := configure(t, st, "/github/push", emulate.Config{Default: emulate.Reply{Status: 200},
		Forward: &emulate.Forward{URL: fr.url() + "/in/hooks"}})
	body := "{\"ref\":\"main\",\"bin\":\"\xff\xfe\"}"
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("POST", url+"/github/push?b=2&a=1", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256=abc")
	req.Header.Add("X-Dup", "1")
	req.Header.Add("X-Dup", "2")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/elsewhere" || string(got) != gz || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("sender got %d %v %q", resp.StatusCode, resp.Header, got)
	}
	if csp := resp.Header.Values("Content-Security-Policy"); len(csp) != 2 || csp[0] != "default-src 'self'" && csp[1] != "default-src 'self'" ||
		!strings.Contains(strings.Join(csp, ","), "sandbox") || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("protection: %v", resp.Header)
	}
	if cs := resp.Header.Values("Set-Cookie"); len(cs) != 1 || cs[0] != "app=1" {
		t.Fatalf("cookies passed back: %v", cs)
	}
	sent := fr.last()
	for _, want := range []string{"POST /in/hooks?b=2&a=1 HTTP/1.1\r\n", "X-Hub-Signature-256: sha256=abc\r\n", "X-Dup: 1\r\nX-Dup: 2\r\n",
		"Host: " + strings.TrimPrefix(fr.url(), "http://") + "\r\n", "\r\n\r\n" + body} {
		if !strings.Contains(sent, want) {
			t.Fatalf("the receiver got\n%q\nwithout %q", sent, want)
		}
	}
	rs := final(t, st, ep, 1)
	o := emulate.ParseOutcome(rs[0].Outcome, rs[0].Status)
	if o.Source != emulate.FromForward || o.Action != emulate.ActForward || o.Status != 302 || o.Forward == nil || o.Forward.Status != 302 ||
		o.Forward.Pending || o.Forward.Target != fr.url()+"/in/hooks" || o.Stamp() != "302 from receiver" || rs[0].Status != 302 {
		t.Fatalf("outcome %s", rs[0].Outcome)
	}
	row, err := st.ForwardOf(context.Background(), rs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != 302 || row.Reason != "Found" || len(row.Headers) != 8 || row.Headers[3].Value != "app=1" || string(row.Body) != gz ||
		row.Line != "POST /in/hooks?b=2&a=1 HTTP/1.1" || row.ErrorKind != "" {
		t.Fatalf("exchange %+v", row)
	}
}

// TestForwardChunked: a chunked original goes on as one chunk, without a
// Content-Length line.
func TestForwardChunked(t *testing.T) {
	fr := newFakeReceiver(t, answering("HTTP/1.1 204 No Content\r\n\r\n"))
	h, st, _ := setup(t, 1<<20)
	// The header lines as wire.Take gives them: a stray Content-Length
	// next to the chunked framing.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Catch(w, r, []wire.Header{{Name: "Host", Value: "x"}, {Name: "Transfer-Encoding", Value: "chunked"}, {Name: "Content-Length", Value: "5"}}, true)
	}))
	t.Cleanup(srv.Close)
	url := srv.URL
	configure(t, st, "/c", emulate.Config{Default: emulate.Reply{Status: 200}, Forward: &emulate.Forward{URL: fr.url() + "/c"}})
	conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "POST /c HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n2\r\nde\r\n0\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("sender got %v %v", resp, err)
	}
	want := "POST /c HTTP/1.1\r\nHost: " + strings.TrimPrefix(fr.url(), "http://") + "\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nabcde\r\n0\r\n\r\n"
	if sent := fr.last(); sent != want {
		t.Fatalf("the receiver got\n%q\nwant\n%q", sent, want)
	}
}

// TestForwardFailures is AC-6: each receiver failure gives the sender the
// failure answer of its endpoint, recorded with its kind.
func TestForwardFailures(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	url := serve(t, h)
	hang := newFakeReceiver(t, func(c net.Conn, _ int32) { time.Sleep(3 * time.Second) })
	big := newFakeReceiver(t, func(c net.Conn, _ int32) {
		fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", 2<<20)
		c.Write(bytes.Repeat([]byte("x"), 2<<20))
	})
	up := newFakeReceiver(t, answering("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"))
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	down := "http://" + ln.Addr().String()
	ln.Close()
	for i, c := range []struct {
		f      emulate.Forward
		status int
		kind   string
		stamp  string
		body   string
	}{
		{emulate.Forward{URL: down}, 502, "dial", "502 receiver down", "could not be reached"},
		{emulate.Forward{URL: hang.url(), TimeoutMs: 200}, 504, "timeout", "504 receiver timeout", "did not answer within the timeout"},
		{emulate.Forward{URL: big.url()}, 502, emulate.FailTooLarge, "502 receiver down", "over the size passed back"},
		{emulate.Forward{URL: up.url()}, 502, emulate.FailUpgrade, "502 receiver down", "switched protocols"},
		{emulate.Forward{URL: "http://169.254.169.254/"}, 502, emulate.FailRefused, "502 receiver refused", "target policy"},
		{emulate.Forward{URL: down, OnFailure: emulate.OnFailureDefault}, 202, "dial", "202 default · receiver down", "fine {{"},
	} {
		path := fmt.Sprintf("/f%d", i)
		f := c.f
		ep := configure(t, st, path, emulate.Config{Default: emulate.Reply{Status: 202, Body: "fine {{\"{{\"}}"}, Forward: &f})
		resp, body, err := do(t, http.DefaultClient, "POST", url+path, "{}")
		if err != nil {
			t.Fatal(err)
		}
		rs := final(t, st, ep, 1)
		o := emulate.ParseOutcome(rs[0].Outcome, rs[0].Status)
		if resp.StatusCode != c.status || !strings.Contains(body, c.body) || o.Stamp() != c.stamp || o.Forward.ErrorKind != c.kind || !o.Forward.Fallback ||
			resp.Header.Get("Content-Security-Policy") != "sandbox" {
			t.Errorf("%s: sender got %d %q; outcome %s (%s)", c.f.URL, resp.StatusCode, body, rs[0].Outcome, o.Stamp())
		}
		if row, err := st.ForwardOf(context.Background(), rs[0].ID); err != nil || row.ErrorKind != c.kind {
			t.Errorf("%s: exchange %+v %v", c.f.URL, row, err)
		}
		if !emulate.Anomalous(o, false, "", false) {
			t.Errorf("%s: a receiver failure is not an anomaly", c.f.URL)
		}
	}
}

// TestForwardChecksAndRules is AC-4 and AC-5: checks refuse before any
// forward; a non-enforced failed verdict is forwarded; a rule takes its
// requests as before and the receiver gets them only with its switch;
// over-limit, off and unmatched requests never reach the receiver.
func TestForwardChecksAndRules(t *testing.T) {
	fr := newFakeReceiver(t, answering("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
	h, st, _ := setup(t, 64)
	url := serve(t, h)
	k := withKeys(t, h)
	cfg := emulate.Config{Default: emulate.Reply{Status: 200}, Forward: &emulate.Forward{URL: fr.url() + "/in"},
		Checks: []emulate.Check{{Header: "X-Event", Match: emulate.MatchPresent}},
		Rules: []emulate.Rule{
			{When: emulate.Condition{Kind: emulate.WhenEvery, N: 3}, Do: emulate.ActReply, Reply: emulate.Reply{Status: 503}, Forward: true},
			{When: emulate.Condition{Kind: emulate.WhenEvery, N: 2}, Do: emulate.ActReply, Reply: emulate.Reply{Status: 500}},
		}}
	ep := configure(t, st, "/r", cfg)
	type step struct {
		hdr                  []string
		body                 string
		status               int
		receiverGot          bool
		stamp, mark, counter string
	}
	for i, s := range []step{
		{nil, "{}", 400, false, "400 refused", "", "0"},                                       // the required header refuses
		{[]string{"X-Event", "a"}, "{}", 200, true, "200 from receiver", "receiver 200", "1"}, // forwarded
		{[]string{"X-Event", "a"}, "{}", 500, false, "500 sent", "not forwarded", "2"},        // every 2nd: not forwarded
		{[]string{"X-Event", "a"}, "{}", 503, true, "503 sent", "receiver 200", "3"},          // every 3rd, the receiver gets it too
		{[]string{"X-Event", "a"}, strings.Repeat("x", 65), 413, false, "413 sent", "", "3"},  // over the limit
	} {
		before := fr.count.Load()
		resp, _, err := do(t, http.DefaultClient, "POST", url+"/r", s.body, s.hdr...)
		if err != nil {
			t.Fatal(err)
		}
		rs := final(t, st, ep, i+1)
		o := emulate.ParseOutcome(rs[0].Outcome, rs[0].Status)
		e, _ := st.Endpoint(context.Background(), ep)
		if resp.StatusCode != s.status || (fr.count.Load() > before) != s.receiverGot || o.Stamp() != s.stamp || o.ReceiverMark() != s.mark ||
			fmt.Sprint(e.Counter) != s.counter {
			t.Errorf("step %d: %d, receiver got it %v, stamp %q, mark %q, counter %d; outcome %s", i+1, resp.StatusCode,
				fr.count.Load() > before, o.Stamp(), o.ReceiverMark(), e.Counter, rs[0].Outcome)
		}
	}
	// A non-enforced authorization check that fails: recorded, forwarded.
	keyed := configure(t, st, "/signed", emulate.Config{Default: emulate.Reply{Status: 200}, Forward: &emulate.Forward{URL: fr.url() + "/in"},
		Checks:  []emulate.Check{{Kind: emulate.KindBearer, Secret: "TOK"}},
		Secrets: []emulate.Secret{{Name: "TOK", Kind: emulate.SecretToken, Hash: k.HashToken("right"), Fingerprint: k.Fingerprint("right")}}})
	before := fr.count.Load()
	if resp, _, _ := do(t, http.DefaultClient, "POST", url+"/signed", "{}", "Authorization", "Bearer wrong"); resp.StatusCode != 200 || fr.count.Load() != before+1 {
		t.Fatalf("a failed, not enforced check: %d, forwarded %v", resp.StatusCode, fr.count.Load() > before)
	}
	if rs := final(t, st, keyed, 1); rs[0].Verdict != string(auth.Fail) {
		t.Fatalf("verdict %q", rs[0].Verdict)
	}
	// Enforced: refused, not forwarded.
	e, _ := st.Endpoint(context.Background(), keyed)
	kc, _ := emulate.Parse(e.Config)
	kc.Checks[0].Enforce = true
	st.SetConfig(context.Background(), keyed, kc.String())
	before = fr.count.Load()
	if resp, _, _ := do(t, http.DefaultClient, "POST", url+"/signed", "{}", "Authorization", "Bearer wrong"); resp.StatusCode != 401 || fr.count.Load() != before {
		t.Fatalf("an enforced check: %d, forwarded %v", resp.StatusCode, fr.count.Load() > before)
	}
	// Off, and a path with no endpoint: 404, not forwarded.
	off := false
	st.UpdateEndpoint(context.Background(), ep, store.EndpointChange{Enabled: &off})
	before = fr.count.Load()
	for _, p := range []string{"/r", "/nowhere"} {
		if resp, _, _ := do(t, http.DefaultClient, "POST", url+p, "{}", "X-Event", "a"); resp.StatusCode != 404 || fr.count.Load() != before {
			t.Fatalf("%s: %d, forwarded %v", p, resp.StatusCode, fr.count.Load() > before)
		}
	}
}

// TestSenderLeavesDuringForward: a sender that gives up does not end the
// exchange; the outcome holds when it left and what the receiver answered.
func TestSenderLeavesDuringForward(t *testing.T) {
	fr := newFakeReceiver(t, func(c net.Conn, _ int32) {
		time.Sleep(400 * time.Millisecond)
		io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	})
	h, st, _ := setup(t, 1<<20)
	url := serve(t, h)
	ep := configure(t, st, "/slow", emulate.Config{Default: emulate.Reply{Status: 200}, Forward: &emulate.Forward{URL: fr.url()}})
	c := &http.Client{Timeout: 100 * time.Millisecond}
	if _, _, err := do(t, c, "POST", url+"/slow", "{}"); err == nil {
		t.Fatal("the sender did not give up")
	}
	rs := final(t, st, ep, 1)
	o := emulate.ParseOutcome(rs[0].Outcome, rs[0].Status)
	if o.Action != emulate.ActAbandoned || o.Would != 200 || o.Forward.Status != 200 || o.Forward.SenderLeftMs < 50 ||
		o.Forward.SenderLeftMs >= o.Forward.Ms || o.Forward.Ms < 400 || o.Stamp() != "sender left" {
		t.Fatalf("outcome %s", rs[0].Outcome)
	}
}

// TestForwardLoop is AC-9's loop: an endpoint that forwards to its own URL
// gets its request back on its own connection and answers it 508; nothing
// is forwarded twice.
func TestForwardLoop(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	url := serve(t, h)
	ep := configure(t, st, "/loop", emulate.Config{Default: emulate.Reply{Status: 200}, Forward: &emulate.Forward{URL: url + "/loop"}})
	resp, body, err := do(t, http.DefaultClient, "POST", url+"/loop", "{}")
	if err != nil {
		t.Fatal(err)
	}
	rs := final(t, st, ep, 2)
	if resp.StatusCode != 508 || !strings.Contains(body, "a loop") || len(rs) != 2 {
		t.Fatalf("sender got %d %q; %d requests", resp.StatusCode, body, len(rs))
	}
	looped, first := emulate.ParseOutcome(rs[0].Outcome, rs[0].Status), emulate.ParseOutcome(rs[1].Outcome, rs[1].Status)
	if looped.Source != emulate.FromLoop || looped.Status != 508 || first.Forward == nil || first.Forward.Status != 508 {
		t.Fatalf("outcomes %s / %s", rs[0].Outcome, rs[1].Outcome)
	}
	// A second endpoint of the same server that does not forward is a fine
	// receiver.
	configure(t, st, "/mock", emulate.Config{Default: emulate.Reply{Status: 201, Body: "mocked"}})
	fw := configure(t, st, "/front", emulate.Config{Default: emulate.Reply{Status: 200}, Forward: &emulate.Forward{URL: url + "/mock"}})
	if resp, body, _ := do(t, http.DefaultClient, "POST", url+"/front", "{}"); resp.StatusCode != 201 || body != "mocked" {
		t.Fatalf("through a mocking endpoint: %d %q", resp.StatusCode, body)
	}
	final(t, st, fw, 1)
}

// TestForwardSlotsAndShutdown: 64 forwards at once; the 65th gets 503 with
// Retry-After; Stop ends the exchanges in flight, each recorded.
func TestForwardSlotsAndShutdown(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	var started atomic.Int32
	h.Forward = forward.New(slog.New(slog.NewTextHandler(io.Discard, nil)), forward.Options{SlotWait: 50 * time.Millisecond,
		Send: func(ctx context.Context, hd wsend.Head, b []byte, o wsend.Options) (wsend.Response, error) {
			started.Add(1)
			<-ctx.Done()
			return wsend.Response{Line: "POST / HTTP/1.1"}, &wsend.Error{Kind: "timeout", Err: ctx.Err()}
		}})
	ep := configure(t, st, "/busy", emulate.Config{Default: emulate.Reply{Status: 200}, Forward: &emulate.Forward{URL: "http://receiver.test/"}})
	var wg sync.WaitGroup
	codes := make(chan int, forward.Slots)
	for range forward.Slots {
		wg.Go(func() { codes <- send(h, "POST", "/busy", strings.NewReader("{}")).Code })
	}
	for started.Load() < forward.Slots {
		time.Sleep(time.Millisecond)
	}
	rec := send(h, "POST", "/busy", strings.NewReader("{}"))
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "5" || !strings.Contains(rec.Body.String(), "too many forwards") {
		t.Fatalf("the 65th: %d %v %q", rec.Code, rec.Header(), rec.Body)
	}
	h.Stop()
	wg.Wait()
	h.Wait()
	close(codes)
	for c := range codes {
		if c != 504 {
			t.Fatalf("a forward ended by the stop answered %d", c)
		}
	}
	rs := final(t, st, ep, forward.Slots+1)
	stopped := 0
	for _, r := range rs {
		if row, err := st.ForwardOf(context.Background(), r.ID); err == nil && row.Error == "timeout: the server stopped before the answer ended" {
			stopped++
		}
	}
	if stopped != forward.Slots {
		t.Fatalf("%d exchanges recorded as stopped, want %d", stopped, forward.Slots)
	}
}

// The answer to a HEAD request has no body, as HTTP requires.
func TestForwardHead(t *testing.T) {
	fr := newFakeReceiver(t, answering("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 5\r\n\r\n"))
	h, st, _ := setup(t, 1<<20)
	configure(t, st, "/h", emulate.Config{Default: emulate.Reply{Status: 200}, Forward: &emulate.Forward{URL: fr.url()}})
	rec := httptest.NewRecorder()
	h.Catch(rec, httptest.NewRequest("HEAD", "/h", nil), nil, false)
	if rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "5" {
		t.Fatalf("HEAD: %d %v %q", rec.Code, rec.Header(), rec.Body)
	}
}

// A handler given no forward service forwards nothing: sending is off.
func TestForwardFailsClosed(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	h.Forward = nil
	fr := newFakeReceiver(t, answering("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
	configure(t, st, "/closed", emulate.Config{Default: emulate.Reply{Status: 200}, Forward: &emulate.Forward{URL: fr.url()}})
	rec := send(h, "POST", "/closed", strings.NewReader("{}"))
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "target policy") || fr.count.Load() != 0 {
		t.Fatalf("%d %q, receiver got %d", rec.Code, rec.Body, fr.count.Load())
	}
}

// TestForwardLogs is AC-12: the logs of forwards, failed or not, name the
// target's origin, never the query, header values or bodies.
func TestForwardLogs(t *testing.T) {
	ok := newFakeReceiver(t, answering("HTTP/1.1 200 OK\r\nX-R: r-secret\r\nContent-Length: 8\r\n\r\nb-secret"))
	h, st, logs := setup(t, 1<<20)
	h.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.Forward = forward.New(h.Log, forward.Options{MaxWait: h.maxWait(), Budget: h})
	url := serve(t, h)
	configure(t, st, "/ok", emulate.Config{Default: emulate.Reply{Status: 200}, Forward: &emulate.Forward{URL: ok.url() + "/p-secret"}})
	configure(t, st, "/down", emulate.Config{Default: emulate.Reply{Status: 200}, Forward: &emulate.Forward{URL: "http://127.0.0.1:1/p-secret"}})
	for _, p := range []string{"/ok", "/down"} {
		do(t, http.DefaultClient, "POST", url+p+"?token=q-secret", `{"s":"s-ecret"}`, "X-Token", "h-secret")
	}
	time.Sleep(100 * time.Millisecond)
	out := logs.String()
	for _, secret := range []string{"q-secret", "h-secret", "s-ecret", "r-secret", "b-secret", "p-secret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("the log has %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, `level=DEBUG msg="request forwarded"`) || !strings.Contains(out, `level=WARN msg="forward failed"`) ||
		!strings.Contains(out, "target=http://127.0.0.1:1\n") {
		t.Fatalf("log:\n%s", out)
	}
}
