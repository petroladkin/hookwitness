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
	if _, ok := c.take("GET", "/b"); ok {
		t.Fatal("a head that does not match the request must not be used")
	}
	if _, ok := c.take("GET", "/b"); ok {
		t.Fatal("after a mismatch the connection must stay off")
	}
}
