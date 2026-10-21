package catch

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/petroladkin/hookwitness/internal/live"
	"github.com/petroladkin/hookwitness/internal/store"
	"github.com/petroladkin/hookwitness/internal/wire"
)

func setup(t *testing.T, maxBody int64) (*Handler, *store.Store, *bytes.Buffer) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var logs bytes.Buffer
	h := &Handler{Store: st, Live: live.New(), Log: slog.New(slog.NewTextHandler(&logs, nil)), MaxBody: maxBody, Retention: 1000}
	return h, st, &logs
}

func send(h *Handler, method, target string, body io.Reader) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.Catch(rec, httptest.NewRequest(method, target, body), nil, false)
	return rec
}

func sendRequest(h *Handler, req *http.Request) *httptest.ResponseRecorder {
	r := httptest.NewRequest(req.Method, req.URL.String(), req.Body)
	r.Header = req.Header
	rec := httptest.NewRecorder()
	h.Catch(rec, r, nil, false)
	return rec
}

func TestAnyMethodToAnEndpoint(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	ep, _ := st.CreateEndpoint(context.Background(), "/hooks/a")
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "PROPFIND"} {
		rec := send(h, m, "/hooks/a?ignored=1", strings.NewReader("x"))
		if rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "0" {
			t.Errorf("%s: %d %q, want 200 with an empty body", m, rec.Code, rec.Body)
		}
	}
	if e, _ := st.Endpoint(context.Background(), ep.ID); e.Count != 8 {
		t.Fatalf("%d requests stored, want 8", e.Count)
	}
}

func TestPathMatchingIsExact(t *testing.T) {
	h, st, logs := setup(t, 1<<20)
	ctx := context.Background()
	st.CreateEndpoint(ctx, "/a")
	st.CreateEndpoint(ctx, "/b/")
	cases := map[string]int{
		"/a":      200,
		"/a?q=1":  200, // query ignored
		"/a/":     404, // trailing slash is significant
		"/b/":     200,
		"/b":      404,
		"/A":      404, // byte for byte
		"/a/x":    404, // no prefix matching
		"//a":     404, // no path cleaning
		"/%61":    404, // no decoding
		"/./a":    404,
		"/nobody": 404,
	}
	for target, want := range cases {
		if got := send(h, "POST", target, nil).Code; got != want {
			t.Errorf("%s: %d, want %d", target, got, want)
		}
	}
	un, _ := st.Requests(ctx, store.Unmatched, 0, 100)
	if len(un) != 8 {
		t.Fatalf("Unmatched register has %d, want 8", len(un))
	}
	if !strings.Contains(logs.String(), "unmatched request") || !strings.Contains(logs.String(), "path=//a") {
		t.Fatalf("unmatched requests not logged:\n%s", logs)
	}
}

func TestStoredFields(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	ctx := context.Background()
	ep, _ := st.CreateEndpoint(ctx, "/p%20q")
	body := []byte("a=1&b=%20\x00\xff binary tail")
	req := httptest.NewRequest("POST", "/p%20q?x=1&x=2&y", bytes.NewReader(body))
	req.RemoteAddr = "198.51.100.7:51000"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.Catch(rec, req, nil, false)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	rs, _ := st.Requests(ctx, ep.ID, 0, 1)
	got, _ := st.Request(ctx, rs[0].ID)
	if !bytes.Equal(got.Body, body) || got.Size != int64(len(body)) {
		t.Fatalf("body %q (size %d), want %q", got.Body, got.Size, body)
	}
	if got.Path != "/p%20q" || got.RawQuery != "x=1&x=2&y" || got.Method != "POST" || got.Proto != "HTTP/1.1" ||
		got.RemoteAddr != "198.51.100.7:51000" || got.Status != 200 || got.TooLarge {
		t.Fatalf("fields: %+v", got)
	}
	if ms := got.ReceivedAt.Nanosecond() % 1_000_000; ms != 0 || got.ReceivedAt.Location().String() != "UTC" {
		t.Fatalf("received at %v: want UTC, millisecond precision", got.ReceivedAt)
	}
	// Without a recorded head the order is Host first, then names sorted.
	if got.Headers[0].Name != "Host" || got.Headers[1].Name != "Content-Type" {
		t.Fatalf("fallback header order: %v", got.Headers)
	}
}

func TestBodyLimit(t *testing.T) {
	h, st, _ := setup(t, 10)
	ctx := context.Background()
	ep, _ := st.CreateEndpoint(ctx, "/x")

	// Exactly at the limit: kept.
	if rec := send(h, "POST", "/x", strings.NewReader("0123456789")); rec.Code != 200 {
		t.Fatalf("at the limit: %d", rec.Code)
	}
	// Declared over the limit: 413 without reading.
	rec := send(h, "POST", "/x", strings.NewReader("0123456789A"))
	if rec.Code != 413 || rec.Header().Get("Connection") != "close" {
		t.Fatalf("over the limit: %d, Connection %q", rec.Code, rec.Header().Get("Connection"))
	}
	// Undeclared (chunked) over the limit: 413 too, size unknown.
	req := httptest.NewRequest("POST", "/x", io.MultiReader(strings.NewReader("0123456789"), strings.NewReader("AB")))
	req.ContentLength = -1
	rec = httptest.NewRecorder()
	h.Catch(rec, req, nil, false)
	if rec.Code != 413 {
		t.Fatalf("chunked over the limit: %d", rec.Code)
	}

	rs, _ := st.Requests(ctx, ep.ID, 0, 10)
	if len(rs) != 3 {
		t.Fatalf("%d stored, want 3 (refused requests are recorded too)", len(rs))
	}
	for i, want := range []int64{-1, 11} {
		r, _ := st.Request(ctx, rs[i].ID)
		if !r.TooLarge || r.Body != nil || r.Size != want || r.Status != 413 {
			t.Fatalf("refused #%d: too_large %v body %q size %d status %d; want size %d", i, r.TooLarge, r.Body, r.Size, r.Status, want)
		}
	}
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestUnreadableBodyIsNotKept(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	ep, _ := st.CreateEndpoint(context.Background(), "/x")
	rec := httptest.NewRecorder()
	h.Catch(rec, httptest.NewRequest("POST", "/x", failingBody{}), nil, false)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if e, _ := st.Endpoint(context.Background(), ep.ID); e.Count != 0 {
		t.Fatal("a request whose body could not be read must not be kept as evidence")
	}
}

func TestPublishesToItsRegister(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	ep, _ := st.CreateEndpoint(context.Background(), "/x")
	wake, stop := h.Live.Subscribe(ep.ID)
	defer stop()
	send(h, "POST", "/x", nil)
	select {
	case <-wake:
	default:
		t.Fatal("no live notification for the endpoint")
	}
}

// A browser logged in to the admin on another port of the same host sends
// its login cookie to catch paths too: its value is never kept.
func TestSessionCookieNeverRecorded(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	ctx := context.Background()
	ep, _ := st.CreateEndpoint(ctx, "/c")
	req := httptest.NewRequest("POST", "/c", strings.NewReader("{}"))
	hs := []wire.Header{{Name: "Host", Value: "x"}, {Name: "cookie", Value: "a=1; hookwitness_session=SECRETSESSIONVALUE; b=2"}, {Name: "Cookie", Value: "c=3"}}
	rec := httptest.NewRecorder()
	h.Catch(rec, req, hs, true)
	rs, _ := st.Requests(ctx, ep.ID, 0, 1)
	got, _ := st.Request(ctx, rs[0].ID)
	if got.Headers[1].Value != "a=1; hookwitness_session=[hookwitness session]; b=2" || got.Headers[2].Value != "c=3" || got.Headers[1].Name != "cookie" {
		t.Fatalf("headers %v", got.Headers)
	}
	for in, want := range map[string]string{
		"hookwitness_session=x":                             "hookwitness_session=[hookwitness session]",
		"hookwitness_session_other=x":                       "hookwitness_session_other=x",
		"a=hookwitness_session":                             "a=hookwitness_session",
		"a=1;hookwitness_session=x;b":                       "a=1;hookwitness_session=[hookwitness session];b",
		"a=1; hookwitness_session=x; hookwitness_session=y": "a=1; hookwitness_session=[hookwitness session]; hookwitness_session=[hookwitness session]",
	} {
		if got := RedactSession(in); got != want {
			t.Errorf("RedactSession(%q) = %q, want %q", in, got, want)
		}
	}
}
