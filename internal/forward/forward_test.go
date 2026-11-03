package forward

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/send"
	"github.com/petroladkin/hookwitness/internal/store"
)

// receiver is a raw TCP listener: it captures each request's bytes (head
// and a Content-Length body) and writes answer.
type receiver struct {
	ln  net.Listener
	mu  sync.Mutex
	got []string
}

func newReceiver(t *testing.T, answer func(c net.Conn)) *receiver {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rc := &receiver{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetReadDeadline(time.Now().Add(time.Second))
				var buf bytes.Buffer
				tmp := make([]byte, 4096)
				for {
					n, err := c.Read(tmp)
					buf.Write(tmp[:n])
					if whole(buf.Bytes()) || err != nil {
						break
					}
				}
				rc.mu.Lock()
				rc.got = append(rc.got, buf.String())
				rc.mu.Unlock()
				c.SetReadDeadline(time.Time{})
				answer(c)
			}()
		}
	}()
	return rc
}

func whole(b []byte) bool {
	i := bytes.Index(b, []byte("\r\n\r\n"))
	if i < 0 {
		return false
	}
	head := strings.ToLower(string(b[:i]))
	if j := strings.Index(head, "content-length: "); j >= 0 {
		var n int
		fmt.Sscanf(head[j+16:], "%d", &n)
		return len(b)-i-4 >= n
	}
	return true
}

func (rc *receiver) url() string { return "http://" + rc.ln.Addr().String() }

func (rc *receiver) last() string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.got) == 0 {
		return ""
	}
	return rc.got[len(rc.got)-1]
}

func quiet() (*slog.Logger, *bytes.Buffer) {
	var b bytes.Buffer
	return slog.New(slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelDebug})), &b
}

var caught = Request{ID: 7, Endpoint: "/github/push", Method: "POST", RawQuery: "token=q-secret&b=2", Proto: "HTTP/1.1",
	Headers: []store.Header{{Name: "X-Hub-Signature-256", Value: "sha256=h-secret"}, {Name: "Host", Value: "hookwitness.test"}, {Name: "x-dup", Value: "1"},
		{Name: "Content-Type", Value: "application/json"}, {Name: "X-Dup", Value: "2"}, {Name: "Content-Length", Value: "15"}},
	Body: []byte(`{"b":"s-ecret"}`)}

// TestForwardsAndPassesBack: the request reaches the receiver byte for byte
// at the target's path plus the received query; the answer comes back
// whole for the sender without framing and hop-by-hop lines, and is kept
// with its lines in order and case.
func TestForwardsAndPassesBack(t *testing.T) {
	rc := newReceiver(t, func(c net.Conn) {
		io.WriteString(c, "HTTP/1.1 201 Made\r\nset-cookie: a=1\r\nConnection: X-Trace, close\r\nX-Trace: t\r\n"+
			"Keep-Alive: timeout=5\r\nContent-Encoding: gzip\r\nSet-Cookie: b=2\r\nClear-Site-Data: \"*\"\r\nStrict-Transport-Security: max-age=1\r\n"+
			"Alt-Svc: h3=\":443\"\r\nContent-Length: 4\r\n\r\n\x1f\x8b\x00\xff")
	})
	log, logs := quiet()
	s := New(log, Options{})
	ex := s.Exchange(context.Background(), emulate.Forward{URL: rc.url() + "/in/hooks"}, caught)
	defer ex.Release()
	if ex.Kind != "" {
		t.Fatalf("kind %q: %s", ex.Kind, ex.Row.Error)
	}
	host := strings.TrimPrefix(rc.url(), "http://")
	want := "POST /in/hooks?token=q-secret&b=2 HTTP/1.1\r\nX-Hub-Signature-256: sha256=h-secret\r\nHost: " + host +
		"\r\nx-dup: 1\r\nContent-Type: application/json\r\nX-Dup: 2\r\nContent-Length: 15\r\n\r\n" + `{"b":"s-ecret"}`
	if got := rc.last(); got != want {
		t.Fatalf("sent\n%q\nwant\n%q", got, want)
	}
	a := ex.Answer
	var names []string
	for _, h := range a.Headers {
		names = append(names, h.Name)
	}
	if a.Status != 201 || string(a.Body) != "\x1f\x8b\x00\xff" || strings.Join(names, ",") != "set-cookie,Content-Encoding,Set-Cookie" {
		t.Fatalf("passed back %d %v %q", a.Status, names, a.Body)
	}
	r := ex.Row
	if r.Status != 201 || r.Reason != "Made" || len(r.Headers) != 10 || r.Headers[0].Name != "set-cookie" || r.Line != "POST /in/hooks?token=q-secret&b=2 HTTP/1.1" ||
		len(r.Sent) != 6 || r.Sent[1].Value != host || r.ErrorKind != "" || r.URL != rc.url()+"/in/hooks" || !strings.Contains(r.Options, `"host":"target"`) {
		t.Fatalf("row %+v", r)
	}
	for _, secret := range []string{"q-secret", "h-secret", "s-ecret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("the log has %q:\n%s", secret, logs)
		}
	}
	if !strings.Contains(logs.String(), "level=DEBUG msg=\"request forwarded\"") || !strings.Contains(logs.String(), "target=http://127.0.0.1:") {
		t.Fatalf("log:\n%s", logs)
	}
	// Host as received, a received query absent.
	q := caught
	q.RawQuery = ""
	ex = s.Exchange(context.Background(), emulate.Forward{URL: rc.url(), KeepHost: true}, q)
	if !strings.HasPrefix(rc.last(), "POST / HTTP/1.1\r\n") || !strings.Contains(rc.last(), "\r\nHost: hookwitness.test\r\n") || !strings.Contains(ex.Row.Options, `"host":"received"`) {
		t.Fatalf("sent %q", rc.last())
	}
}

// TestAnswerSizes: a 200 KiB answer passes back whole and is kept to
// 64 KiB; one over 1 MiB is a failure.
func TestAnswerSizes(t *testing.T) {
	body := func(n int) func(net.Conn) {
		return func(c net.Conn) {
			fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", n)
			c.Write(bytes.Repeat([]byte("x"), n))
		}
	}
	log, _ := quiet()
	s := New(log, Options{})
	rc := newReceiver(t, body(200<<10))
	ex := s.Exchange(context.Background(), emulate.Forward{URL: rc.url()}, caught)
	if ex.Kind != "" || len(ex.Answer.Body) != 200<<10 || len(ex.Row.Body) != KeepBody || !ex.Row.Truncated || ex.Row.Size != 200<<10 {
		t.Fatalf("200 KiB: kind %q, passed %d, kept %d truncated %v size %d", ex.Kind, len(ex.Answer.Body), len(ex.Row.Body), ex.Row.Truncated, ex.Row.Size)
	}
	rc = newReceiver(t, body(2<<20))
	ex = s.Exchange(context.Background(), emulate.Forward{URL: rc.url()}, caught)
	if ex.Kind != emulate.FailTooLarge || ex.Row.Status != 200 || len(ex.Row.Body) != KeepBody || !strings.Contains(ex.Row.Error, "over 1024 KiB") {
		t.Fatalf("2 MiB: kind %q status %d kept %d %q", ex.Kind, ex.Row.Status, len(ex.Row.Body), ex.Row.Error)
	}
}

// TestFailures: a 101, a reset, a refused connection, a timeout, and the
// target policy, each recorded with its kind.
func TestFailures(t *testing.T) {
	log, logs := quiet()
	s := New(log, Options{})
	up := newReceiver(t, func(c net.Conn) { io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n\r\n") })
	reset := newReceiver(t, func(c net.Conn) {
		io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial")
		c.(*net.TCPConn).SetLinger(0)
	})
	hang := newReceiver(t, func(c net.Conn) { time.Sleep(2 * time.Second) })
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := "http://" + ln.Addr().String()
	ln.Close()
	for _, c := range []struct {
		f    emulate.Forward
		kind string
		why  string
	}{
		{emulate.Forward{URL: up.url()}, emulate.FailUpgrade, "101 Switching Protocols"},
		{emulate.Forward{URL: reset.url()}, "reset", "reset"},
		{emulate.Forward{URL: closed}, "dial", "dial"},
		{emulate.Forward{URL: hang.url(), TimeoutMs: 200}, "timeout", "timeout"},
		{emulate.Forward{URL: "http://169.254.169.254/latest"}, emulate.FailRefused, "link-local"},
	} {
		ex := s.Exchange(context.Background(), c.f, caught)
		ex.Release()
		if ex.Kind != c.kind || ex.Row.ErrorKind != c.kind || !strings.Contains(ex.Row.Error, c.why) {
			t.Errorf("%s: kind %q %q", c.f.URL, ex.Kind, ex.Row.Error)
		}
	}
	if !strings.Contains(logs.String(), "level=WARN msg=\"forward failed\"") || strings.Contains(logs.String(), "/latest") {
		t.Fatalf("log:\n%s", logs)
	}
}

// staticResolver resolves every name to addrs.
type staticResolver []netip.Addr

func (r staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return r, nil
}

// TestPolicy: a name that resolves to a metadata or link-local address is
// refused on the dialled address; "none" turns forwarding off; a narrowed
// list refuses the rest.
func TestPolicy(t *testing.T) {
	log, _ := quiet()
	for _, a := range []string{"169.254.169.254", "fe80::1", "fd00:ec2::254"} {
		s := New(log, Options{Resolver: staticResolver{netip.MustParseAddr(a)}})
		ex := s.Exchange(context.Background(), emulate.Forward{URL: "http://receiver.test/in"}, caught)
		if ex.Kind != emulate.FailRefused || ex.Row.Line != "" {
			t.Errorf("%s: %q %q", a, ex.Kind, ex.Row.Error)
		}
	}
	off, _ := send.ParsePolicy("none")
	s := New(log, Options{Policy: off})
	if ex := s.Exchange(context.Background(), emulate.Forward{URL: "http://receiver.test/in"}, caught); ex.Kind != emulate.FailRefused ||
		ex.Row.Error != "forwarding is off on this server (-replay-allow none)" {
		t.Errorf("off: %q %q", ex.Kind, ex.Row.Error)
	}
	only, _ := send.ParsePolicy("receiver.test")
	s = New(log, Options{Policy: only, Resolver: staticResolver{netip.MustParseAddr("192.0.2.10")}})
	if ex := s.Exchange(context.Background(), emulate.Forward{URL: "http://other.test/in"}, caught); ex.Kind != emulate.FailRefused {
		t.Errorf("narrowed: %q %q", ex.Kind, ex.Row.Error)
	}
}

// TestSlotsBudgetAndStop: 64 exchanges at once; the 65th waits for a slot
// and gets busy; an answer needs its bytes held; Stop ends what is in
// flight as a timeout; the local ends are the service's own until they end.
func TestSlotsBudgetAndStop(t *testing.T) {
	log, _ := quiet()
	var started atomic.Int32
	release := make(chan struct{})
	block := func(ctx context.Context, h send.Head, b []byte, o send.Options) (send.Response, error) {
		o.OnConnect(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000 + int(started.Add(1))})
		select {
		case <-release:
			return send.Response{Status: 204, Line: "POST /in HTTP/1.1"}, nil
		case <-ctx.Done():
			return send.Response{Line: "POST /in HTTP/1.1"}, &send.Error{Kind: "timeout", Err: ctx.Err()}
		}
	}
	s := New(log, Options{Send: block, SlotWait: 50 * time.Millisecond})
	f := emulate.Forward{URL: "http://receiver.test/in"}
	var wg sync.WaitGroup
	results := make(chan Exchange, Slots)
	for range Slots {
		wg.Go(func() { results <- s.Exchange(context.Background(), f, caught) })
	}
	for started.Load() < Slots {
		time.Sleep(time.Millisecond)
	}
	if !s.Own("127.0.0.1:40001") || s.Own("127.0.0.1:39999") {
		t.Fatal("the connections in flight are not the service's own")
	}
	if ex := s.Exchange(context.Background(), f, caught); ex.Kind != emulate.FailBusy || !strings.Contains(ex.Row.Error, "64 forward slots") {
		t.Fatalf("the 65th: %q %q", ex.Kind, ex.Row.Error)
	}
	close(release)
	wg.Wait()
	close(results)
	for ex := range results {
		if ex.Kind != "" || ex.Answer.Status != 204 {
			t.Fatalf("in a slot: %q %d", ex.Kind, ex.Answer.Status)
		}
	}
	if s.Own("127.0.0.1:40001") {
		t.Fatal("an ended connection is still the service's own")
	}

	// The answer's bytes are held until released; none free: busy.
	b := &budget{free: MaxAnswer}
	s = New(log, Options{Budget: b, Send: func(context.Context, send.Head, []byte, send.Options) (send.Response, error) {
		return send.Response{Status: 200}, nil
	}})
	ex := s.Exchange(context.Background(), f, caught)
	if ex.Kind != "" || b.free != 0 {
		t.Fatalf("held: %q, %d free", ex.Kind, b.free)
	}
	if ex2 := s.Exchange(context.Background(), f, caught); ex2.Kind != emulate.FailBusy || !strings.Contains(ex2.Row.Error, "-max-inflight") {
		t.Fatalf("no bytes free: %q %q", ex2.Kind, ex2.Row.Error)
	}
	ex.Release()
	ex.Release()
	if b.free != MaxAnswer {
		t.Fatalf("after release: %d free", b.free)
	}

	// Stop ends an exchange in flight, recorded as a timeout.
	release = make(chan struct{})
	s = New(log, Options{Send: block})
	done := make(chan Exchange)
	go func() { done <- s.Exchange(context.Background(), f, caught) }()
	time.Sleep(20 * time.Millisecond)
	s.Stop()
	ex = <-done
	s.Wait()
	if ex.Kind != "timeout" || ex.Row.Error != "timeout: the server stopped before the answer ended" {
		t.Fatalf("stopped: %q %q", ex.Kind, ex.Row.Error)
	}
	if ex := s.Exchange(context.Background(), f, caught); ex.Kind != "timeout" {
		t.Fatalf("after Stop: %q", ex.Kind)
	}
}

type budget struct {
	mu   sync.Mutex
	free int64
}

func (b *budget) Reserve(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.free {
		return false
	}
	b.free -= n
	return true
}

func (b *budget) Release(n int64) {
	b.mu.Lock()
	b.free += n
	b.mu.Unlock()
}

// TestSenderLeavingDoesNotCancel: the caller's context ending does not end
// the exchange.
func TestSenderLeavingDoesNotCancel(t *testing.T) {
	log, _ := quiet()
	s := New(log, Options{Send: func(ctx context.Context, h send.Head, b []byte, o send.Options) (send.Response, error) {
		time.Sleep(50 * time.Millisecond)
		if ctx.Err() != nil {
			return send.Response{}, &send.Error{Kind: "timeout", Err: ctx.Err()}
		}
		return send.Response{Status: 200}, nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ex := s.Exchange(ctx, emulate.Forward{URL: "http://receiver.test/in"}, caught); ex.Kind != "" || ex.Answer.Status != 200 {
		t.Fatalf("a gone caller: %q %d", ex.Kind, ex.Answer.Status)
	}
}

// A receiver with a certificate the server does not trust fails the TLS
// handshake, unless the forward accepts any certificate.
func TestTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	t.Cleanup(srv.Close)
	log, _ := quiet()
	s := New(log, Options{})
	if ex := s.Exchange(context.Background(), emulate.Forward{URL: srv.URL + "/in"}, caught); ex.Kind != "tls" {
		t.Fatalf("an untrusted certificate: %q %q", ex.Kind, ex.Row.Error)
	}
	ex := s.Exchange(context.Background(), emulate.Forward{URL: srv.URL + "/in", InsecureTLS: true}, caught)
	if ex.Kind != "" || ex.Answer.Status != 204 || !strings.Contains(ex.Row.Options, `"insecure_tls":true`) {
		t.Fatalf("any certificate accepted: %q %d", ex.Kind, ex.Answer.Status)
	}
}
