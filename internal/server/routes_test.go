package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/catch"
	"github.com/petroladkin/hookwitness/internal/live"
	"github.com/petroladkin/hookwitness/internal/store"
	"github.com/petroladkin/hookwitness/internal/wire"
)

type recordingCatcher struct{ paths []string }

func (c *recordingCatcher) Catch(w http.ResponseWriter, r *http.Request, _ []wire.Header, _ bool) {
	c.paths = append(c.paths, r.RequestURI)
	w.WriteHeader(299)
}

func TestRoutesOnTheRawPath(t *testing.T) {
	c := &recordingCatcher{}
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(298) })
	h := New(Options{DB: pinger{}, Log: quiet, AdminPrefix: "/admin", Admin: admin, Catch: c})
	for target, want := range map[string]int{
		"/healthz":        200,
		"/admin":          298,
		"/admin/":         298,
		"/admin/e/1":      298,
		"/administrator":  299, // not under the prefix
		"/healthz/x":      299,
		"//admin":         299, // no cleaning: this is not the admin path
		"/a/../admin/x":   299,
		"/stripe/webhook": 299,
		"/":               299,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", target, rec.Code, want)
		}
	}
	if len(c.paths) == 0 || c.paths[0] == "" {
		t.Fatal("catcher got no requests")
	}
}

// serveReal runs the real stack (store, catcher) on a loopback listener.
func serveReal(t *testing.T) (addr string, st *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{DB: st, Log: quiet, AdminPrefix: "/admin",
		Catch: &catch.Handler{Store: st, Live: live.New(), Log: quiet, MaxBody: 1 << 20, Retention: 1000}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, h, quiet) }()
	t.Cleanup(func() { cancel(); <-done })
	return ln.Addr().String(), st
}

// TestHeadersKeepReceivedOrder is AC-3 end to end: header lines reach the
// store in the order, case and repetition they were sent, on a kept-alive
// connection carrying several requests.
func TestHeadersKeepReceivedOrder(t *testing.T) {
	addr, st := serveReal(t)
	ep, _ := st.CreateEndpoint(context.Background(), "/order")
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	heads := []string{
		"POST /order?b=2&a=1 HTTP/1.1\r\nHost: example.test\r\nx-zeta: 1\r\nX-Alpha: a\r\nx-zeta: 2\r\nContent-Length: 3\r\n\r\nabc",
		"GET /admin/ HTTP/1.1\r\nHost: example.test\r\n\r\n", // not caught, but its head is taken
		"PUT /order HTTP/1.1\r\nhost: example.test\r\nTransfer-Encoding: chunked\r\nlower-case: v\r\n\r\n2\r\nhi\r\n0\r\n\r\n",
	}
	for _, h := range heads {
		io.WriteString(c, h)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	rs, _ := st.Requests(context.Background(), ep.ID, 0, 10)
	if len(rs) != 2 {
		t.Fatalf("%d stored, want 2", len(rs))
	}
	want := []string{
		"[{host example.test} {Transfer-Encoding chunked} {lower-case v}]",
		"[{Host example.test} {x-zeta 1} {X-Alpha a} {x-zeta 2} {Content-Length 3}]",
	}
	for i, r := range rs {
		if got := fmt.Sprint(r.Headers); got != want[i] {
			t.Errorf("request %d headers %s, want %s", r.No, got, want[i])
		}
	}
	full, _ := st.Request(context.Background(), rs[0].ID)
	if string(full.Body) != "hi" {
		t.Errorf("chunked body %q, want hi", full.Body)
	}
}

// TestConcurrentRequestsAllKept is AC-9: 100 concurrent requests to one
// endpoint over HTTP, every one stored.
func TestConcurrentRequestsAllKept(t *testing.T) {
	addr, st := serveReal(t)
	ep, _ := st.CreateEndpoint(context.Background(), "/burst")
	client := &http.Client{Timeout: 10 * time.Second}
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := range 100 {
		wg.Go(func() {
			resp, err := client.Post("http://"+addr+"/burst", "application/json", strings.NewReader(fmt.Sprintf(`{"n":%d}`, i)))
			if err != nil {
				errs <- err
				return
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				errs <- fmt.Errorf("status %d", resp.StatusCode)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	rs, _ := st.Requests(context.Background(), ep.ID, 0, 1000)
	bodies := map[string]bool{}
	for _, r := range rs {
		full, _ := st.Request(context.Background(), r.ID)
		bodies[string(full.Body)] = true
	}
	if len(rs) != 100 || len(bodies) != 100 {
		t.Fatalf("%d stored with %d distinct bodies, want 100 and 100", len(rs), len(bodies))
	}
}

func TestOnShutdownRuns(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	called := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, http.NotFoundHandler(), quiet, func() { close(called) }) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("onShutdown function not called")
	}
}
