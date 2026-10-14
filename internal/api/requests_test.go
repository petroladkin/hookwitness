package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/store"
)

type requestList struct{ Requests []summaryJSON }

func ids(l requestList) []int64 {
	out := []int64{}
	for _, r := range l.Requests {
		out = append(out, r.ID)
	}
	return out
}

func (e env) list(t *testing.T, query string) requestList {
	t.Helper()
	rec := e.do("GET", "/endpoints/1/requests"+query, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list %s: %d %s", query, rec.Code, rec.Body)
	}
	return decodeAs[requestList](t, rec)
}

func TestListRequests(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	other := e.endpoint(t, "/other")
	for i := range 5 {
		e.add(t, ep.ID, fmt.Sprintf(`{"n":%d}`, i), nil)
	}
	e.add(t, other.ID, `{}`, nil)

	if got := ids(e.list(t, "")); fmt.Sprint(got) != "[5 4 3 2 1]" {
		t.Fatalf("newest first: %v", got)
	}
	if got := ids(e.list(t, "?limit=2")); fmt.Sprint(got) != "[5 4]" {
		t.Fatalf("limit: %v", got)
	}
	if got := ids(e.list(t, "?before=4&limit=2")); fmt.Sprint(got) != "[3 2]" {
		t.Fatalf("before: %v", got)
	}
	if got := ids(e.list(t, "?after=2")); fmt.Sprint(got) != "[3 4 5]" {
		t.Fatalf("after, oldest first: %v", got)
	}
	if got := ids(e.list(t, "?after=0&limit=2")); fmt.Sprint(got) != "[1 2]" {
		t.Fatalf("after with a limit: %v", got)
	}
	if got := ids(e.list(t, "?after=5")); fmt.Sprint(got) != "[]" {
		t.Fatalf("nothing newer: %v", got)
	}
	if got := ids(e.list(t, "?limit=100000")); len(got) != 5 {
		t.Fatalf("a large limit is capped, not refused: %v", got)
	}
	s := e.list(t, "?limit=1").Requests[0]
	if s.EndpointID == nil || *s.EndpointID != ep.ID || s.No != 5 || s.Method != "POST" || s.ContentType != "application/json" ||
		s.BodySize != 7 || !s.BodyKept || s.Status != 200 || time.Time(s.ReceivedAt).IsZero() {
		t.Fatalf("summary %+v", s)
	}

	for q, field := range map[string]string{
		"?limit=0": "limit", "?limit=x": "limit", "?after=-1": "after", "?before=x": "before",
		"?after=1&before=3": "before", "?wait=5s": "wait", "?after=0&wait=61s": "wait", "?after=0&wait=soon": "wait",
	} {
		wantError(t, e.do("GET", "/endpoints/1/requests"+q, ""), http.StatusUnprocessableEntity, "invalid", field)
	}
	wantError(t, e.do("GET", "/endpoints/9/requests", ""), http.StatusNotFound, "not_found", "")
}

func TestGetRequest(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	r := e.add(t, ep.ID, `{"event":"push"}`, func(r *store.Request) {
		r.RawQuery = "a=1&a=2"
		r.Headers = []store.Header{{Name: "content-type", Value: "application/json"}, {Name: "X-Dup", Value: "1"},
			{Name: "Authorization", Value: "Basic c2VuZGVyOnB3"}, {Name: "x-dup", Value: "2"}}
		r.Verdict = "pass"
		r.Verdicts = `[{"check":1,"label":"Basic credentials","result":"pass","reason":"user and password match","at":"2026-10-13T18:48:01.38Z"}]`
		r.Outcome = `{"action":"reply","status":200}`
	})
	rec := e.do("GET", fmt.Sprintf("/requests/%d", r.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	got := decodeAs[map[string]any](t, rec)
	if fmt.Sprint(got["headers"]) != "[[content-type application/json] [X-Dup 1] [Authorization Basic c2VuZGVyOnB3] [x-dup 2]]" {
		t.Fatalf("headers as received: %v", got["headers"])
	}
	if got["body_text"] != `{"event":"push"}` || got["body_base64"] != nil || got["query"] != "a=1&a=2" || got["verdict"] != "pass" {
		t.Fatalf("request %v", got)
	}
	if fmt.Sprint(got["verdicts"]) != "[map[at:2026-10-13T18:48:01.380Z check:1 label:Basic credentials reason:user and password match result:pass]]" || fmt.Sprint(got["outcome"]) != "map[action:reply status:200]" {
		t.Fatalf("verdicts %v, outcome %v", got["verdicts"], got["outcome"])
	}

	bin := e.add(t, ep.ID, "\xff\x00\xfe", nil)
	got = decodeAs[map[string]any](t, e.do("GET", fmt.Sprintf("/requests/%d", bin.ID), ""))
	if got["body_base64"] != base64.StdEncoding.EncodeToString([]byte("\xff\x00\xfe")) || got["body_text"] != nil {
		t.Fatalf("binary body: %v", got)
	}
	big := e.add(t, ep.ID, "", func(r *store.Request) { r.TooLarge, r.Size, r.Status = true, 99999999, 413 })
	got = decodeAs[map[string]any](t, e.do("GET", fmt.Sprintf("/requests/%d", big.ID), ""))
	if got["body_kept"] != false || got["body_text"] != nil || got["body_base64"] != nil || got["status"] != float64(413) ||
		fmt.Sprint(got["verdicts"]) != "[]" || got["outcome"] != nil {
		t.Fatalf("refused body: %v", got)
	}
	un := e.add(t, store.Unmatched, "x", func(r *store.Request) { r.Path, r.Status = "/nowhere", 404 })
	got = decodeAs[map[string]any](t, e.do("GET", fmt.Sprintf("/requests/%d", un.ID), ""))
	if got["endpoint_id"] != nil || got["path"] != "/nowhere" {
		t.Fatalf("unmatched: %v", got)
	}
	wantError(t, e.do("GET", "/requests/999", ""), http.StatusNotFound, "not_found", "")
}

// waitFor runs a waiting list call in the background.
func (e env) waitFor(ctx context.Context, query string) <-chan requestList {
	out := make(chan requestList, 1)
	go func() {
		rec := e.call(ctx, "GET", "/endpoints/1/requests"+query, "")
		var l requestList
		json.Unmarshal(rec.Body.Bytes(), &l) // a client that left gets no body
		out <- l
	}()
	return out
}

func TestWaitAnswersAtOnceWhenNewerExists(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	e.add(t, ep.ID, `{}`, nil)
	start := time.Now()
	if got := ids(e.list(t, "?after=0&wait=30s")); fmt.Sprint(got) != "[1]" || time.Since(start) > 5*time.Second {
		t.Fatalf("got %v after %v", got, time.Since(start))
	}
}

func TestWaitWakesOnANewRequest(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	e.add(t, ep.ID, `{"old":1}`, nil)
	done := e.waitFor(t.Context(), "?after=1&wait=30s")
	time.Sleep(100 * time.Millisecond)
	select {
	case l := <-done:
		t.Fatalf("answered before a new request: %v", ids(l))
	default:
	}
	e.add(t, store.Unmatched, `{}`, nil) // another register: not a wake-up for this one
	e.add(t, ep.ID, `{"new":1}`, nil)
	select {
	case l := <-done:
		if len(l.Requests) != 1 || l.Requests[0].No != 2 {
			t.Fatalf("woke with %+v", l)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("not woken by a new request")
	}
}

func TestWaitEndsEmptyAtTheDeadline(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t, "/hooks")
	start := time.Now()
	l := e.list(t, "?after=0&wait=300ms")
	if took := time.Since(start); len(l.Requests) != 0 || took < 300*time.Millisecond {
		t.Fatalf("%v after %v", ids(l), took)
	}
	if l := e.list(t, "?after=0&wait=0"); len(l.Requests) != 0 {
		t.Fatalf("wait=0: %v", ids(l))
	}
}

func TestWaitEndsWhenTheClientLeavesOrTheServerStops(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t, "/hooks")

	ctx, cancel := context.WithCancel(t.Context())
	left := e.waitFor(ctx, "?after=0&wait=60s")
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-left:
	case <-time.After(10 * time.Second):
		t.Fatal("a call whose client left kept waiting")
	}

	stopped := e.waitFor(t.Context(), "?after=0&wait=60s")
	time.Sleep(50 * time.Millisecond)
	close(e.stop)
	select {
	case l := <-stopped:
		if l.Requests == nil || len(l.Requests) != 0 {
			t.Fatalf("on shutdown: %+v, want an empty list", l)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a call kept waiting through shutdown")
	}
}

func TestManyWaitersAllWake(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	const n = 50
	var wg sync.WaitGroup
	got := make(chan int, n)
	for range n {
		wg.Go(func() {
			rec := e.do("GET", "/endpoints/1/requests?after=0&wait=30s", "")
			got <- len(decodeAs[requestList](t, rec).Requests)
		})
	}
	time.Sleep(200 * time.Millisecond)
	e.add(t, ep.ID, `{}`, nil)
	wg.Wait()
	close(got)
	for c := range got {
		if c != 1 {
			t.Fatalf("a waiter got %d requests, want 1", c)
		}
	}
}

func TestRequestImages(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/uploads")
	// A 1×1 PNG, declared by a schema verdict at /photo and sent as a data URL at /extra.
	png1 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=="
	r := e.add(t, ep.ID, `{"photo":"`+png1+`","extra":"data:image/png;base64,`+png1+`"}`, func(r *store.Request) {
		r.Verdicts = `[{"check":1,"label":"JSON Schema Upload","result":"pass","images":[{"path":"/photo","declared":"image/png","type":"image/png","size":70,"width":1,"height":1}]}]`
	})
	got := decodeAs[map[string]any](t, e.do("GET", fmt.Sprintf("/requests/%d", r.ID), ""))
	imgs, _ := got["images"].([]any)
	if len(imgs) != 2 {
		t.Fatalf("images %v", got["images"])
	}
	first := imgs[0].(map[string]any)
	if first["path"] != "/extra" || first["source"] != "data URL" || first["type"] != "image/png" ||
		first["url"] != fmt.Sprintf("/admin/api/v1/requests/%d/image?path=%%2Fextra", r.ID) {
		t.Fatalf("first image %v", first)
	}
	if second := imgs[1].(map[string]any); second["source"] != "schema" || second["declared"] != "image/png" {
		t.Fatalf("second image %v", second)
	}
	rec := e.do("GET", fmt.Sprintf("/requests/%d/image?path=/photo", r.ID), "")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" || !bytes.HasPrefix(rec.Body.Bytes(), []byte("\x89PNG")) {
		t.Fatalf("bytes: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	wantError(t, e.do("GET", fmt.Sprintf("/requests/%d/image?path=/nope", r.ID), ""), http.StatusNotFound, "not_found", "path")
	plain := e.add(t, ep.ID, `{"note":"aGVsbG8="}`, nil)
	wantError(t, e.do("GET", fmt.Sprintf("/requests/%d/image?path=/note", plain.ID), ""), http.StatusUnprocessableEntity, "not_an_image", "path")
	if got := decodeAs[map[string]any](t, e.do("GET", fmt.Sprintf("/requests/%d", plain.ID), "")); fmt.Sprint(got["images"]) != "[]" {
		t.Fatalf("no images: %v", got["images"])
	}
}
