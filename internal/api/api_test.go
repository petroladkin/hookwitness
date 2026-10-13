package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/live"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
)

type env struct {
	h    *Handler
	st   *store.Store
	lv   *live.Broker
	log  *syncBuffer
	stop chan struct{}
}

// syncBuffer collects the log; handlers may write from several goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newEnv(t *testing.T) env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	keys, err := secret.Load("", filepath.Join(t.TempDir(), "hw.db.key"))
	if err != nil {
		t.Fatal(err)
	}
	lv := live.New()
	logs := &syncBuffer{}
	stop := make(chan struct{})
	h := New(st, lv, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Options{Prefix: "/admin", MaxWait: time.Minute, Keys: keys, Keyring: keys, Stop: stop, Version: "v0.0.0-test"})
	return env{h, st, lv, logs, stop}
}

// call sends a request to the API; path is relative to /admin/api/v1. A
// body is sent as JSON unless a Content-Type is given in hdr.
func (e env) call(ctx context.Context, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequestWithContext(ctx, method, "/admin/api/v1"+path, rd)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	return rec
}

func (e env) do(method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	return e.call(context.Background(), method, path, body, hdr...)
}

func decodeAs[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("answer is not JSON (%v): %s", err, rec.Body)
	}
	return v
}

type apiErr struct {
	Error struct{ Code, Message, Field string }
}

// wantError checks an error answer: status, code and (if given) field.
func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code, field string) apiErr {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
	e := decodeAs[apiErr](t, rec)
	if e.Error.Code != code || e.Error.Message == "" || (field != "" && e.Error.Field != field) {
		t.Fatalf("error %+v, want code %s field %q", e.Error, code, field)
	}
	return e
}

func (e env) endpoint(t *testing.T, path string) endpointJSON {
	t.Helper()
	rec := e.do("POST", "/endpoints", `{"path":"`+path+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s: %d %s", path, rec.Code, rec.Body)
	}
	return decodeAs[endpointJSON](t, rec)
}

// add stores a caught request and wakes waiters, as the catcher does.
func (e env) add(t *testing.T, endpointID int64, body string, mod func(*store.Request)) *store.Request {
	t.Helper()
	r := &store.Request{EndpointID: endpointID, ReceivedAt: time.Now(), Method: "POST", Path: "/x", Proto: "HTTP/1.1",
		RemoteAddr: "203.0.113.9:4000", Headers: []store.Header{{Name: "Content-Type", Value: "application/json"}},
		Body: []byte(body), Size: int64(len(body)), Status: 200}
	if mod != nil {
		mod(r)
	}
	if err := e.st.AddRequest(context.Background(), r, 1000, nil); err != nil {
		t.Fatal(err)
	}
	e.lv.Publish(endpointID)
	return r
}

func TestWritesNeedJSON(t *testing.T) {
	e := newEnv(t)
	wantError(t, e.do("POST", "/endpoints", `path=/x`, "Content-Type", "application/x-www-form-urlencoded"),
		http.StatusUnsupportedMediaType, "unsupported_media_type", "")
	wantError(t, e.do("POST", "/endpoints", ""), http.StatusUnsupportedMediaType, "unsupported_media_type", "")
	wantError(t, e.do("PUT", "/endpoints/1/behaviour", `{}`, "Content-Type", "text/plain"),
		http.StatusUnsupportedMediaType, "unsupported_media_type", "")
	if rec := e.do("POST", "/endpoints", `{"path":"/x"}`, "Content-Type", "application/json; charset=utf-8"); rec.Code != http.StatusCreated {
		t.Fatalf("JSON with a charset: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/endpoints", ""); rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("a read needs no content type: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

func TestUnknownRoute(t *testing.T) {
	e := newEnv(t)
	wantError(t, e.do("GET", "/nope", ""), http.StatusNotFound, "not_found", "")
	wantError(t, e.do("DELETE", "/endpoints", ""), http.StatusNotFound, "not_found", "")
}

func TestBadBodies(t *testing.T) {
	e := newEnv(t)
	wantError(t, e.do("POST", "/endpoints", `{"path":`), http.StatusBadRequest, "bad_json", "")
	wantError(t, e.do("POST", "/endpoints", `{"path":"/a","colour":"red"}`), http.StatusBadRequest, "unknown_field", "colour")
	wantError(t, e.do("POST", "/endpoints", `{"path":5}`), http.StatusBadRequest, "bad_json", "path")
	wantError(t, e.do("POST", "/endpoints", `{"path":"/a"} {"path":"/b"}`), http.StatusBadRequest, "bad_json", "")
	wantError(t, e.do("POST", "/endpoints", `{"path":"`+strings.Repeat("a", maxBody)+`"}`), http.StatusRequestEntityTooLarge, "too_large", "")
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
