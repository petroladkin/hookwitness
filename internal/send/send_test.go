package send

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/wire"
)

// target is a raw TCP listener: it captures the bytes of one request (up to
// the end of the head plus want body bytes) and writes answer.
type target struct {
	ln     net.Listener
	mu     sync.Mutex
	got    []byte
	answer func(c net.Conn, req []byte)
}

func newTarget(t *testing.T, answer func(c net.Conn, req []byte)) *target {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tg := &target{ln: ln, answer: answer}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
				var buf bytes.Buffer
				tmp := make([]byte, 4096)
				for {
					n, err := c.Read(tmp)
					buf.Write(tmp[:n])
					if complete(buf.Bytes()) || err != nil {
						break
					}
				}
				tg.mu.Lock()
				tg.got = append([]byte(nil), buf.Bytes()...)
				tg.mu.Unlock()
				c.SetReadDeadline(time.Time{})
				tg.answer(c, buf.Bytes())
			}()
		}
	}()
	return tg
}

// complete reports whether b holds a whole request (Content-Length or
// chunked framing).
func complete(b []byte) bool {
	i := bytes.Index(b, []byte("\r\n\r\n"))
	if i < 0 {
		return false
	}
	head, body := strings.ToLower(string(b[:i])), b[i+4:]
	if strings.Contains(head, "transfer-encoding: chunked") {
		return bytes.HasSuffix(body, []byte("0\r\n\r\n"))
	}
	if j := strings.Index(head, "content-length: "); j >= 0 {
		var n int
		fmt.Sscanf(head[j+16:], "%d", &n)
		return len(body) >= n
	}
	return true
}

func (tg *target) captured() string {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return string(tg.got)
}

func (tg *target) url(t *testing.T) *url.URL {
	u, err := url.Parse("http://" + tg.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func answerWith(s string) func(net.Conn, []byte) {
	return func(c net.Conn, _ []byte) { io.WriteString(c, s) }
}

var okAnswer = answerWith("HTTP/1.1 204 No Content\r\n\r\n")

// TestBytesAsReceived: names keep their case, repeats and interleaving, the
// body's bytes are untouched, and nothing is added.
func TestBytesAsReceived(t *testing.T) {
	tg := newTarget(t, okAnswer)
	u := tg.url(t)
	body := []byte{0xff, 0x00, 'a', '\r', '\n', 0xfe}
	h := Head{Method: "POST", Target: "/hook/%7Euser?b=2&a=1", Headers: []wire.Header{
		{Name: "x-hub-signature-256", Value: "sha256=ab"},
		{Name: "Host", Value: "hookwitness.test:8080"},
		{Name: "X-Dup", Value: "1"},
		{Name: "content-type", Value: "application/octet-stream"},
		{Name: "x-dup", Value: "2"},
		{Name: "Content-Length", Value: "6"},
		{Name: "X-Empty", Value: ""},
	}}
	res, err := Send(context.Background(), h, body, Options{URL: u})
	if err != nil {
		t.Fatal(err)
	}
	want := "POST /hook/%7Euser?b=2&a=1 HTTP/1.1\r\n" +
		"x-hub-signature-256: sha256=ab\r\n" +
		"Host: " + u.Host + "\r\n" +
		"X-Dup: 1\r\n" +
		"content-type: application/octet-stream\r\n" +
		"x-dup: 2\r\n" +
		"Content-Length: 6\r\n" +
		"X-Empty: \r\n" +
		"\r\n" + string(body)
	if got := tg.captured(); got != want {
		t.Fatalf("sent\n%q\nwant\n%q", got, want)
	}
	if res.Status != 204 || res.Line != "POST /hook/%7Euser?b=2&a=1 HTTP/1.1" || len(res.Sent) != 7 || res.Sent[1].Value != u.Host || len(res.Notes) != 0 {
		t.Fatalf("response %+v", res)
	}

	// KeepHost sends the received value.
	if _, err := Send(context.Background(), h, body, Options{URL: u, KeepHost: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tg.captured(), "\r\nHost: hookwitness.test:8080\r\n") {
		t.Fatalf("KeepHost sent %q", tg.captured())
	}
}

func TestFraming(t *testing.T) {
	tg := newTarget(t, okAnswer)
	u := tg.url(t)
	// A chunked original goes as one chunk and the last chunk.
	res, err := Send(context.Background(), Head{"POST", "/c", []wire.Header{{Name: "Host", Value: "x"}, {Name: "Transfer-Encoding", Value: "chunked"}}},
		[]byte("hello world"), Options{URL: u})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tg.captured(), "POST /c HTTP/1.1\r\nHost: "+u.Host+"\r\nTransfer-Encoding: chunked\r\n\r\nb\r\nhello world\r\n0\r\n\r\n"; got != want {
		t.Fatalf("chunked sent %q, want %q", got, want)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "one chunk") {
		t.Fatalf("notes %q", res.Notes)
	}
	// An empty chunked body is the last chunk only.
	Send(context.Background(), Head{"POST", "/c", []wire.Header{{Name: "Host", Value: "x"}, {Name: "Transfer-Encoding", Value: "chunked"}}}, nil, Options{URL: u})
	if got := tg.captured(); !strings.HasSuffix(got, "chunked\r\n\r\n0\r\n\r\n") {
		t.Fatalf("empty chunked sent %q", got)
	}
	// An HTTP/1.0 request without Host gets one; the request line says 1.1.
	res, err = Send(context.Background(), Head{"GET", "/old", []wire.Header{{Name: "Accept", Value: "*/*"}}}, nil, Options{URL: u})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := tg.captured(), "GET /old HTTP/1.1\r\nHost: "+u.Host+"\r\nAccept: */*\r\n\r\n"; got != want {
		t.Fatalf("sent %q, want %q", got, want)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "Host added") {
		t.Fatalf("notes %q", res.Notes)
	}
	// Lines that cannot be sent are refused, never written.
	for _, h := range []Head{
		{"PO ST", "/", nil}, {"POST", "/a b", nil}, {"POST", "/", []wire.Header{{Name: "X-A", Value: "1\r\nX-Injected: 1"}}},
		{"POST", "/", []wire.Header{{Name: "Bad Name", Value: "1"}}},
	} {
		if _, err := Send(context.Background(), h, nil, Options{URL: u}); err == nil {
			t.Errorf("%+v sent", h)
		}
	}
}

func TestAnswer(t *testing.T) {
	gz := func() []byte {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		w.Write([]byte("compressed"))
		w.Close()
		return b.Bytes()
	}()
	big := bytes.Repeat([]byte("x"), 2<<20)
	for _, tc := range []struct {
		name   string
		answer string
		check  func(t *testing.T, res Response, err error)
	}{
		{"headers in order and case", "HTTP/1.1 401 Unauthorized\r\nx-b: 1\r\nX-A: 2\r\nx-b: 3\r\nContent-Length: 3\r\n\r\nno!", func(t *testing.T, res Response, err error) {
			if err != nil || res.Status != 401 || res.Reason != "Unauthorized" || string(res.Body) != "no!" || res.Size != 3 || res.Truncated {
				t.Fatalf("%+v %v", res, err)
			}
			if fmt.Sprint(res.Headers) != "[{x-b 1} {X-A 2} {x-b 3} {Content-Length 3}]" {
				t.Fatalf("headers %v", res.Headers)
			}
		}},
		{"gzip kept compressed", "HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: " + fmt.Sprint(len(gz)) + "\r\n\r\n" + string(gz), func(t *testing.T, res Response, err error) {
			if err != nil || !bytes.Equal(res.Body, gz) {
				t.Fatalf("body %q err %v", res.Body, err)
			}
		}},
		{"redirect not followed", "HTTP/1.1 302 Found\r\nLocation: http://169.254.169.254/\r\nContent-Length: 0\r\n\r\n", func(t *testing.T, res Response, err error) {
			if err != nil || res.Status != 302 {
				t.Fatalf("%+v %v", res, err)
			}
		}},
		{"chunked answer", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n2\r\nde\r\n0\r\n\r\n", func(t *testing.T, res Response, err error) {
			if err != nil || string(res.Body) != "abcde" || res.Size != 5 {
				t.Fatalf("%+v %v", res, err)
			}
		}},
		{"until close", "HTTP/1.0 200 OK\r\n\r\nall of it", func(t *testing.T, res Response, err error) {
			if err != nil || string(res.Body) != "all of it" || res.Proto != "HTTP/1.0" {
				t.Fatalf("%+v %v", res, err)
			}
		}},
		{"interim 100 skipped", "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 201 Created\r\nX: y\r\nContent-Length: 0\r\n\r\n", func(t *testing.T, res Response, err error) {
			if err != nil || res.Status != 201 || fmt.Sprint(res.Headers) != "[{X y} {Content-Length 0}]" {
				t.Fatalf("%+v %v", res, err)
			}
		}},
		{"over 1 MiB", "HTTP/1.1 200 OK\r\nContent-Length: " + fmt.Sprint(len(big)) + "\r\n\r\n" + string(big), func(t *testing.T, res Response, err error) {
			if err != nil || len(res.Body) != DefaultMaxBody || res.Size != int64(len(big)) || !res.Truncated {
				t.Fatalf("kept %d of %d, truncated %v, err %v", len(res.Body), res.Size, res.Truncated, err)
			}
		}},
		{"cut short", "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nonly this", func(t *testing.T, res Response, err error) {
			var e *Error
			if !errors.As(err, &e) || e.Kind != "reset" || res.Status != 200 || res.Size != -1 || string(res.Body) != "only this" {
				t.Fatalf("%+v %v", res, err)
			}
		}},
		{"not HTTP", "SSH-2.0-OpenSSH\r\n\r\n", func(t *testing.T, res Response, err error) {
			var e *Error
			if !errors.As(err, &e) || e.Kind != "answer" || !strings.Contains(err.Error(), "not HTTP") {
				t.Fatalf("%v", err)
			}
		}},
		{"closed without answer", "", func(t *testing.T, res Response, err error) {
			var e *Error
			if !errors.As(err, &e) || e.Kind != "reset" || res.Status != 0 {
				t.Fatalf("%v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tg := newTarget(t, answerWith(tc.answer))
			res, err := Send(context.Background(), Head{"POST", "/", []wire.Header{{Name: "Host", Value: "x"}, {Name: "Content-Length", Value: "2"}}}, []byte("{}"), Options{URL: tg.url(t)})
			tc.check(t, res, err)
		})
	}
}

func TestTimeoutAndReset(t *testing.T) {
	hang := newTarget(t, func(c net.Conn, _ []byte) { time.Sleep(3 * time.Second) })
	start := time.Now()
	res, err := Send(context.Background(), Head{"GET", "/", nil}, nil, Options{URL: hang.url(t), Timeout: 200 * time.Millisecond})
	var e *Error
	if !errors.As(err, &e) || e.Kind != "timeout" || res.Status != 0 {
		t.Fatalf("hang: %v %+v", err, res)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("the timeout was not applied: %s", time.Since(start))
	}
	// Headers sent, then the body trickles past the timeout.
	slow := newTarget(t, func(c net.Conn, _ []byte) {
		io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nab")
		time.Sleep(3 * time.Second)
	})
	res, err = Send(context.Background(), Head{"GET", "/", nil}, nil, Options{URL: slow.url(t), Timeout: 300 * time.Millisecond})
	if !errors.As(err, &e) || e.Kind != "timeout" || res.Status != 200 || res.Size != -1 || string(res.Body) != "ab" || res.TTFB <= 0 {
		t.Fatalf("slow: %v %+v", err, res)
	}
	reset := newTarget(t, func(c net.Conn, _ []byte) {
		c.(*net.TCPConn).SetLinger(0)
		c.Close()
	})
	_, err = Send(context.Background(), Head{"GET", "/", nil}, nil, Options{URL: reset.url(t)})
	if !errors.As(err, &e) || e.Kind != "reset" {
		t.Fatalf("reset: %v", err)
	}
	// Nothing listens.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	u, _ := url.Parse("http://" + addr)
	_, err = Send(context.Background(), Head{"GET", "/", nil}, nil, Options{URL: u})
	if !errors.As(err, &e) || e.Kind != "dial" {
		t.Fatalf("closed port: %v", err)
	}
}

type stubResolver map[string][]netip.Addr

func (s stubResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := s[host]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// TestPolicyAtDial: a name that resolves to a denied address is refused
// before any connection; a mix is narrowed to the allowed addresses; and
// the dialer's own hook refuses even when the address list was allowed.
func TestPolicyAtDial(t *testing.T) {
	tg := newTarget(t, okAnswer)
	port := tg.ln.Addr().(*net.TCPAddr).Port
	res := stubResolver{
		"metadata.test":  {netip.MustParseAddr("169.254.169.254")},
		"linklocal.test": {netip.MustParseAddr("fe80::1")},
		"mapped.test":    {netip.MustParseAddr("::ffff:169.254.169.254")},
		"mixed.test":     {netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("127.0.0.1")},
	}
	for _, host := range []string{"metadata.test", "linklocal.test", "mapped.test"} {
		u, _ := url.Parse(fmt.Sprintf("http://%s:%d/", host, port))
		_, err := Send(context.Background(), Head{"GET", "/", nil}, nil, Options{URL: u, Resolver: res})
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), host+" resolves to") || !strings.Contains(err.Error(), "link-local") {
			t.Errorf("%s: %v", host, err)
		}
	}
	u, _ := url.Parse(fmt.Sprintf("http://mixed.test:%d/", port))
	if r, err := Send(context.Background(), Head{"GET", "/", nil}, nil, Options{URL: u, Resolver: res}); err != nil || r.Status != 204 {
		t.Fatalf("mixed: %v", err)
	}
	// The resolver changes its answer between the check and the connect
	// (DNS rebinding): the Control hook sees the address really dialled.
	var dialled []string
	o := Options{URL: u, Resolver: res}
	o.dialControl = func(network, address string, c syscall.RawConn) error {
		dialled = append(dialled, address)
		return o.Policy.CheckAddr("mixed.test", port, netip.MustParseAddr("169.254.169.254"))
	}
	_, err := Send(context.Background(), Head{"GET", "/", nil}, nil, o)
	if !errors.Is(err, ErrRefused) || len(dialled) != 1 {
		t.Fatalf("control hook: %v, dialled %v", err, dialled)
	}
	// The default hook parses the real address and checks it.
	o.dialControl = nil
	d := net.Dialer{Control: func(network, address string, c syscall.RawConn) error {
		ap := netip.MustParseAddrPort(address)
		return Policy{}.CheckAddr("", int(ap.Port()), ap.Addr())
	}}
	if _, err := d.Dial("tcp", "169.254.169.254:80"); !errors.Is(err, ErrRefused) {
		t.Fatalf("hook on a literal: %v", err)
	}
	// none: nothing is dialled.
	off, _ := ParsePolicy("none")
	if _, err := Send(context.Background(), Head{"GET", "/", nil}, nil, Options{URL: tg.url(t), Policy: off}); !errors.Is(err, ErrRefused) {
		t.Fatalf("none: %v", err)
	}
	// An allowlist that does not list the target.
	only, _ := ParsePolicy("handler.test")
	if _, err := Send(context.Background(), Head{"GET", "/", nil}, nil, Options{URL: tg.url(t), Policy: only}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "-replay-allow") {
		t.Fatalf("allowlist: %v", err)
	}
}

func TestTLS(t *testing.T) {
	var proto string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto = r.Proto
		w.Header().Set("X-Seen-Host", r.Host)
		w.WriteHeader(202)
	}))
	srv.EnableHTTP2 = true // the server offers h2; the sender must not take it
	srv.StartTLS()
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	h := Head{"POST", "/x", []wire.Header{{Name: "Host", Value: "h"}, {Name: "Content-Length", Value: "0"}}}

	_, err := Send(context.Background(), h, nil, Options{URL: u})
	var e *Error
	if !errors.As(err, &e) || e.Kind != "tls" {
		t.Fatalf("an untrusted certificate: %v", err)
	}
	res, err := Send(context.Background(), h, nil, Options{URL: u, InsecureTLS: true})
	if err != nil || res.Status != 202 || proto != "HTTP/1.1" {
		t.Fatalf("insecure: %v %+v %s", err, res, proto)
	}
	res, err = Send(context.Background(), h, nil, Options{URL: u, RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs})
	if err != nil || res.Status != 202 || proto != "HTTP/1.1" {
		t.Fatalf("trusted: %v %+v %s", err, res, proto)
	}
}
