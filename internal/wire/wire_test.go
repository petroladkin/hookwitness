package wire

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func feedAll(t *testing.T, stream string, chunk int) ([]head, bool) {
	t.Helper()
	var p parser
	var got []head
	b := []byte(stream)
	for len(b) > 0 {
		n := min(chunk, len(b))
		p.feed(b[:n], func(h head) { got = append(got, h) })
		b = b[n:]
	}
	return got, p.broken
}

func TestParserFollowsFraming(t *testing.T) {
	stream := "POST /a?x=1 HTTP/1.1\r\nHost: h\r\nx-b: 1\r\nX-A: 2\r\nx-b: 3\r\nContent-Length: 11\r\n\r\nGET / HTTP\r\n" +
		"\r\n" + // a stray blank line between requests is tolerated
		"POST /c HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n4;ext=1\r\nPOST\r\n6\r\n /x HT\r\n0\r\nTrailer: t\r\n\r\n" +
		"POST /d HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n" +
		"GET /e HTTP/1.0\r\nFolded: one\r\n  two\r\n\r\n"
	for _, chunk := range []int{1, 3, 7, 4096} {
		t.Run(fmt.Sprint("chunk", chunk), func(t *testing.T) {
			got, broken := feedAll(t, stream, chunk)
			if broken {
				t.Fatal("parser gave up")
			}
			var lines []string
			for _, h := range got {
				lines = append(lines, h.line)
			}
			want := []string{"POST /a?x=1 HTTP/1.1", "POST /c HTTP/1.1", "POST /d HTTP/1.1", "GET /e HTTP/1.0"}
			if fmt.Sprint(lines) != fmt.Sprint(want) {
				t.Fatalf("request lines %q, want %q (a body that looks like a request must be skipped)", lines, want)
			}
			if fmt.Sprint(got[0].headers) != "[{Host h} {x-b 1} {X-A 2} {x-b 3} {Content-Length 11}]" {
				t.Fatalf("headers %v", got[0].headers)
			}
			if fmt.Sprint(got[3].headers) != "[{Folded one two}]" {
				t.Fatalf("folded header %v", got[3].headers)
			}
		})
	}
}

func TestParserGivesUp(t *testing.T) {
	for name, stream := range map[string]string{
		"not http":       "\x16\x03\x01 TLS hello\r\n\r\n",
		"bad length":     "POST / HTTP/1.1\r\nContent-Length: x\r\n\r\n",
		"bad chunk":      "POST / HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\n",
		"no colon":       "GET / HTTP/1.1\r\nnonsense\r\n\r\n",
		"head too large": "GET / HTTP/1.1\r\nX: " + strings.Repeat("a", maxHead) + "\r\n\r\n",
	} {
		if _, broken := feedAll(t, stream, 512); !broken {
			t.Errorf("%s: parser did not give up", name)
		}
	}
}

// TestTakeThroughServer runs a real http.Server: pipelined requests on one
// connection each get their own headers, in received order.
func TestTakeThroughServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string
	srv := &http.Server{
		ConnContext: ConnContext,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hs, ok := Take(r)
			io.Copy(io.Discard, r.Body)
			mu.Lock()
			seen = append(seen, fmt.Sprint(r.URL.Path, " ", ok, " ", hs))
			mu.Unlock()
		}),
	}
	go srv.Serve(Listener(ln))
	defer srv.Close()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "POST /one HTTP/1.1\r\nHost: h\r\nzeta: 1\r\nAlpha: 2\r\nzeta: 3\r\nContent-Length: 5\r\n\r\nhello"+
		"POST /two HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\nB: x\r\n\r\n3\r\nabc\r\n0\r\n\r\n"+
		"GET /three HTTP/1.1\r\nHost: h\r\nConnection: close\r\n\r\n")
	br := bufio.NewReader(c)
	for range 3 {
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	want := []string{
		"/one true [{Host h} {zeta 1} {Alpha 2} {zeta 3} {Content-Length 5}]",
		"/two true [{Host h} {Transfer-Encoding chunked} {B x}]",
		"/three true [{Host h} {Connection close}]",
	}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Fatalf("got\n %q\nwant\n %q", seen, want)
	}
}

func TestTakeWithoutRecording(t *testing.T) {
	r, _ := http.NewRequest("GET", "/", nil)
	if _, ok := Take(r); ok {
		t.Fatal("Take on a request without a recording connection must report !ok")
	}
	c := &Conn{}
	c.p.feed([]byte("GET /a HTTP/1.1\r\nA: 1\r\n\r\nGET /b HTTP/1.1\r\n\r\n"), func(h head) { c.heads = append(c.heads, h) })
	if _, ok := c.take("GET", "/b", http.Header{}); ok {
		t.Fatal("a head that does not match the request must not be used")
	}
	if _, ok := c.take("GET", "/b", http.Header{}); ok {
		t.Fatal("after a mismatch the connection must stay off")
	}
}

// serveAndTake runs raw requests through a real http.Server on one
// connection and returns, per request, its path, Take's ok and headers.
func serveAndTake(t *testing.T, raw string, responses int) []string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string
	srv := &http.Server{
		ConnContext: ConnContext,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hs, ok := Take(r)
			io.Copy(io.Discard, r.Body)
			mu.Lock()
			seen = append(seen, fmt.Sprint(r.URL.Path, " ", ok, " ", hs))
			mu.Unlock()
		}),
	}
	go srv.Serve(Listener(ln))
	defer srv.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, raw)
	br := bufio.NewReader(c)
	for range responses {
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			break
		}
		resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	return seen
}

// TestFramingAgreesWithNetHTTP: where wire and net/http could frame a stream
// differently, a request must never get headers that are not its own.
func TestFramingAgreesWithNetHTTP(t *testing.T) {
	smuggled := "0\r\n\r\nGET /y HTTP/1.1\r\nHost: a\r\nX-Forged: yes\r\n\r\n"
	cases := map[string]struct {
		raw  string
		n    int
		want []string
	}{
		// net/http ignores Transfer-Encoding on HTTP/1.0 and reads Content-Length.
		"HTTP/1.0 with Transfer-Encoding": {
			"POST /x HTTP/1.0\r\nConnection: keep-alive\r\nTransfer-Encoding: chunked\r\nContent-Length: " +
				fmt.Sprint(len(smuggled)) + "\r\n\r\n" + smuggled +
				"GET /y HTTP/1.1\r\nHost: a\r\nX-Real: 1\r\nConnection: close\r\n\r\n", 2,
			[]string{
				"/x true [{Connection keep-alive} {Transfer-Encoding chunked} {Content-Length 48}]",
				"/y true [{Host a} {X-Real 1} {Connection close}]",
			},
		},
		// A head ending in bare LFs, with CRLF CRLF inside its body.
		"bare LF head": {
			"POST /x HTTP/1.1\nHost: a\nContent-Length: 30\n\nX-Forged: yes\r\n\r\nabcdefghijklmn" +
				"GET /y HTTP/1.1\r\nHost: a\r\nConnection: close\r\n\r\n", 2,
			[]string{
				"/x true [{Host a} {Content-Length 30}]",
				"/y true [{Host a} {Connection close}]",
			},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := serveAndTake(t, c.raw, c.n)
			for i, g := range got {
				if strings.Contains(g, "X-Forged") {
					t.Fatalf("request %d got forged headers: %s", i, g)
				}
				if i < len(c.want) && strings.Contains(g, " true ") && g != c.want[i] {
					t.Errorf("request %d: %s, want %s (or ok=false)", i, g, c.want[i])
				}
			}
		})
	}
}

func TestSameHeaders(t *testing.T) {
	rec := []Header{{"host", "h"}, {"x-a", "1"}, {"X-B", "2"}, {"x-a", "3"}, {"Content-Length", "4"}}
	parsed := http.Header{"X-A": {"1", "3"}, "X-B": {"2"}, "Content-Length": {"4"}}
	if !sameHeaders(rec, parsed) {
		t.Fatal("equal headers reported different")
	}
	parsed["X-C"] = []string{"extra"}
	if sameHeaders(rec, parsed) {
		t.Fatal("a header only net/http saw must be a mismatch")
	}
	delete(parsed, "X-C")
	parsed["X-A"] = []string{"3", "1"}
	if sameHeaders(rec, parsed) {
		t.Fatal("different value order must be a mismatch")
	}
}

// TestDrippedHeadIsLinear: a head fed one byte at a time must not be
// rescanned from its start on every read.
func TestDrippedHeadIsLinear(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nX: " + strings.Repeat("a", 512<<10) + "\r\n\r\n"
	start := time.Now()
	var p parser
	n := 0
	for i := range len(raw) {
		p.feed([]byte{raw[i]}, func(head) { n++ })
	}
	if n != 1 || p.broken {
		t.Fatalf("heads %d, broken %v", n, p.broken)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("512 KiB head fed byte by byte took %v", d)
	}
}
