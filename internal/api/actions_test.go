package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/petroladkin/hookwitness/internal/store"
)

func TestPatchAndDeleteEndpoint(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/old")
	e.endpoint(t, "/taken")
	e.add(t, ep.ID, `{}`, nil)
	rec := e.do("PATCH", fmt.Sprintf("/endpoints/%d", ep.ID), `{"path":"/new/place"}`)
	got := decodeAs[endpointJSON](t, rec)
	if rec.Code != 200 || got.Path != "/new/place" || got.Requests != 1 || !strings.HasSuffix(got.URL, "/new/place") {
		t.Fatalf("patch: %d %+v", rec.Code, got)
	}
	wantError(t, e.do("PATCH", fmt.Sprintf("/endpoints/%d", ep.ID), `{"path":"/taken"}`), http.StatusConflict, "duplicate_path", "path")
	wantError(t, e.do("PATCH", fmt.Sprintf("/endpoints/%d", ep.ID), `{"path":"no-slash"}`), http.StatusUnprocessableEntity, "invalid", "path")
	wantError(t, e.do("PATCH", fmt.Sprintf("/endpoints/%d", ep.ID), `{"path":"/admin/x"}`), http.StatusUnprocessableEntity, "invalid", "path")
	wantError(t, e.do("PATCH", fmt.Sprintf("/endpoints/%d", ep.ID), `{}`), http.StatusUnprocessableEntity, "invalid", "path")
	wantError(t, e.do("PATCH", fmt.Sprintf("/endpoints/%d", ep.ID), `{"name":"x"}`), http.StatusBadRequest, "unknown_field", "name")
	if rec := e.do("PATCH", fmt.Sprintf("/endpoints/%d", ep.ID), `{"path":"/new/place"}`); rec.Code != 200 {
		t.Fatalf("the same path again: %d", rec.Code)
	}

	if rec := e.do("DELETE", fmt.Sprintf("/endpoints/%d", ep.ID), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	wantError(t, e.do("GET", fmt.Sprintf("/endpoints/%d", ep.ID), ""), http.StatusNotFound, "not_found", "")
	wantError(t, e.do("DELETE", fmt.Sprintf("/endpoints/%d", ep.ID), ""), http.StatusNotFound, "not_found", "")
}

func TestClearResetRecheck(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	e.add(t, ep.ID, `{"a":1}`, func(r *store.Request) { r.Headers = append(r.Headers, store.Header{Name: "X-Token", Value: "t"}) })
	e.st.AddRequest(t.Context(), &store.Request{EndpointID: ep.ID, Method: "POST", Path: "/hooks", Body: []byte("{}"), Status: 200}, 1000,
		func(n int64) (int, string) { return 200, `{"action":"reply","status":200}` })
	// A required-header check would not give verdicts; a basic check does.
	e.do("PUT", fmt.Sprintf("/endpoints/%d/secrets/PW", ep.ID), `{"kind":"token","value":"test-pw"}`)
	_, tag := e.behaviour(t, ep.ID)
	if rec := e.do("PUT", fmt.Sprintf("/endpoints/%d/behaviour", ep.ID), `{"checks":[{"kind":"basic","secret":"PW","user":"u"}]}`, "If-Match", tag); rec.Code != 200 {
		t.Fatalf("behaviour: %d %s", rec.Code, rec.Body)
	}
	rec := e.do("POST", fmt.Sprintf("/endpoints/%d/recheck", ep.ID), "{}")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "{\n  \"rechecked\": 2\n}" {
		t.Fatalf("recheck: %d %s", rec.Code, rec.Body)
	}
	list := e.list(t, "")
	if len(list.Requests) != 2 || list.Requests[0].Verdict != "fail" {
		t.Fatalf("verdicts after re-check: %+v", list.Requests)
	}
	if got := decodeAs[endpointJSON](t, e.do("GET", fmt.Sprintf("/endpoints/%d", ep.ID), "")); got.Counter != 1 {
		t.Fatalf("counter before reset: %d", got.Counter)
	}
	rec = e.do("POST", fmt.Sprintf("/endpoints/%d/reset-counter", ep.ID), "{}")
	if got := decodeAs[endpointJSON](t, rec); rec.Code != 200 || got.Counter != 0 {
		t.Fatalf("reset: %d %+v", rec.Code, got)
	}
	// Writes need JSON: a form post from another site cannot trigger them.
	wantError(t, e.do("POST", fmt.Sprintf("/endpoints/%d/recheck", ep.ID), "x=1", "Content-Type", "application/x-www-form-urlencoded"),
		http.StatusUnsupportedMediaType, "unsupported_media_type", "")
	if rec := e.do("DELETE", fmt.Sprintf("/endpoints/%d/requests", ep.ID), ""); rec.Code != http.StatusNoContent {
		t.Fatalf("clear: %d", rec.Code)
	}
	if l := e.list(t, ""); len(l.Requests) != 0 {
		t.Fatalf("after clear: %+v", l.Requests)
	}
	wantError(t, e.do("DELETE", "/endpoints/99/requests", ""), http.StatusNotFound, "not_found", "")
}

func TestRawBody(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	html := e.add(t, ep.ID, `<script>alert(1)</script>`, func(r *store.Request) {
		r.Headers = []store.Header{{Name: "Content-Type", Value: "text/html; charset=utf-8"}}
	})
	rec := e.do("GET", fmt.Sprintf("/requests/%d/body", html.ID), "")
	h := rec.Header()
	if rec.Code != 200 || rec.Body.String() != `<script>alert(1)</script>` || h.Get("Content-Type") != "text/html; charset=utf-8" ||
		!strings.HasPrefix(h.Get("Content-Disposition"), "attachment;") || h.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("raw body: %d %v", rec.Code, h)
	}
	odd := e.add(t, ep.ID, "\x00\x01", func(r *store.Request) { r.Headers = []store.Header{{Name: "Content-Type", Value: "not a type"}} })
	if ct := e.do("GET", fmt.Sprintf("/requests/%d/body", odd.ID), "").Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("an invalid Content-Type: %q", ct)
	}
	big := e.add(t, ep.ID, "", func(r *store.Request) { r.TooLarge, r.Size, r.Status = true, 1<<30, 413 })
	wantError(t, e.do("GET", fmt.Sprintf("/requests/%d/body", big.ID), ""), http.StatusNotFound, "not_kept", "")
	wantError(t, e.do("GET", "/requests/999/body", ""), http.StatusNotFound, "not_found", "")
}

func TestUnmatchedAndFilters(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	for _, v := range []struct {
		verdict string
		status  int
	}{{"pass", 200}, {"fail", 401}, {"pass", 503}, {"n/c", 200}, {"", 0}} {
		e.add(t, ep.ID, `{}`, func(r *store.Request) { r.Verdict, r.Status = v.verdict, v.status })
	}
	for i := range 3 {
		e.add(t, store.Unmatched, "", func(r *store.Request) { r.Path, r.Status = fmt.Sprintf("/typo%d", i), 404 })
	}
	nos := func(query string) string {
		var out []string
		for _, r := range e.list(t, query).Requests {
			out = append(out, fmt.Sprint(r.No))
		}
		return strings.Join(out, ",")
	}
	for q, want := range map[string]string{
		"?verdict=pass":              "3,1",
		"?verdict=n/c":               "4",
		"?status=200":                "4,1",
		"?status=0":                  "5",
		"?verdict=pass&status=503":   "3",
		"?verdict=pass&limit=1":      "3",
		"?after=1&verdict=pass":      "3",
		"?after=0&status=200&wait=0": "1,4",
		"?before=3&verdict=fail":     "2",
	} {
		if got := nos(q); got != want {
			t.Errorf("%s: %s, want %s", q, got, want)
		}
	}
	wantError(t, e.do("GET", fmt.Sprintf("/endpoints/%d/requests?verdict=ok", ep.ID), ""), http.StatusUnprocessableEntity, "invalid", "verdict")
	wantError(t, e.do("GET", fmt.Sprintf("/endpoints/%d/requests?status=abc", ep.ID), ""), http.StatusUnprocessableEntity, "invalid", "status")

	un := decodeAs[requestList](t, e.do("GET", "/unmatched/requests?limit=2", ""))
	if len(un.Requests) != 2 || un.Requests[0].Path != "/typo2" || un.Requests[0].EndpointID != nil {
		t.Fatalf("unmatched: %+v", un.Requests)
	}
	if after := decodeAs[requestList](t, e.do("GET", fmt.Sprintf("/unmatched/requests?after=%d", un.Requests[1].ID), "")); len(after.Requests) != 1 {
		t.Fatalf("unmatched after: %+v", after.Requests)
	}
	if rec := e.do("POST", "/unmatched/requests", "{}"); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unmatched is read-only: %d", rec.Code)
	}
}

func TestReportRoute(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/orders")
	e.add(t, ep.ID, `{"n":1}`, func(r *store.Request) {
		r.Headers = append(r.Headers, store.Header{Name: "Authorization", Value: "Bearer test-token-123"})
	})
	e.add(t, ep.ID, `{"n":2}`, nil)
	for f, ct := range map[string]string{"md": "text/markdown; charset=utf-8", "html": "text/html; charset=utf-8", "pdf": "application/pdf"} {
		rec := e.do("GET", fmt.Sprintf("/endpoints/%d/report?format=%s", ep.ID, f), "")
		if rec.Code != 200 || rec.Header().Get("Content-Type") != ct || !strings.Contains(rec.Header().Get("Content-Disposition"), "hookwitness-orders-") {
			t.Fatalf("%s: %d %v", f, rec.Code, rec.Header())
		}
	}
	md := e.do("GET", fmt.Sprintf("/endpoints/%d/report?format=md&scope=picked&pick=1&mask=Authorization", ep.ID), "").Body.String()
	if !strings.Contains(md, "| Selection | request #1 |") || strings.Contains(md, "test-token-123") || !strings.Contains(md, "Generated | ") ||
		!strings.Contains(md, "by Hookwitness v0.0.0-test") || strings.Contains(md, "### #2") {
		t.Fatalf("picked, masked:\n%s", md)
	}
	rng := e.do("GET", fmt.Sprintf("/endpoints/%d/report?format=md&scope=range&from=2000-01-01T00:00:00Z&to=2000-01-02T00:00:00Z", ep.ID), "").Body.String()
	if !strings.Contains(rng, "| Requests | 0 |") {
		t.Fatal("an RFC 3339 range")
	}
	wantError(t, e.do("GET", fmt.Sprintf("/endpoints/%d/report?format=doc", ep.ID), ""), http.StatusUnprocessableEntity, "invalid", "format")
	wantError(t, e.do("GET", fmt.Sprintf("/endpoints/%d/report?format=md&scope=picked&pick=x", ep.ID), ""), http.StatusUnprocessableEntity, "invalid", "pick")
	wantError(t, e.do("GET", "/endpoints/99/report?format=md", ""), http.StatusNotFound, "not_found", "")
	e.add(t, store.Unmatched, "", func(r *store.Request) { r.Path, r.Status = "/typo", 404 })
	if un := e.do("GET", "/unmatched/report?format=md", ""); un.Code != 200 || !strings.Contains(un.Body.String(), "# Hookwitness report · Unmatched") {
		t.Fatalf("unmatched report: %d", un.Code)
	}
}

// A credential a sender sent without its scheme reaches no verdict reason,
// so a report with Authorization masked keeps it out entirely.
func TestMaskedReportKeepsBareCredentialsOut(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/pay")
	for _, v := range []string{"sk_test_51Hsecret", "tok123 extra"} {
		e.add(t, ep.ID, `{}`, func(r *store.Request) { r.Headers = append(r.Headers, store.Header{Name: "Authorization", Value: v}) })
	}
	e.do("PUT", fmt.Sprintf("/endpoints/%d/secrets/TOK", ep.ID), `{"kind":"token","value":"tok_test"}`)
	_, tag := e.behaviour(t, ep.ID)
	if rec := e.do("PUT", fmt.Sprintf("/endpoints/%d/behaviour", ep.ID), `{"checks":[{"kind":"bearer","secret":"TOK"}]}`, "If-Match", tag); rec.Code != 200 {
		t.Fatalf("behaviour: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", fmt.Sprintf("/endpoints/%d/recheck", ep.ID), "{}"); rec.Code != 200 {
		t.Fatalf("recheck: %d", rec.Code)
	}
	for _, f := range []string{"md", "html"} {
		out := e.do("GET", fmt.Sprintf("/endpoints/%d/report?format=%s&mask=Authorization", ep.ID, f), "").Body.String()
		if !strings.Contains(out, "scheme is not Bearer") || strings.Contains(out, "sk_test_51Hsecret") || strings.Contains(out, "tok123") {
			t.Errorf("%s report:\n%s", f, out)
		}
	}
}

// A report that fails before its first byte (the store is gone) is an error
// answer, not 200 with an empty file.
func TestReportFailsBeforeWriting(t *testing.T) {
	e := newEnv(t)
	e.st.Close()
	for _, f := range []string{"md", "html", "pdf"} {
		rec := e.do("GET", "/unmatched/report?format="+f, "")
		if rec.Header().Get("Content-Disposition") != "" {
			t.Errorf("%s: a download was started: %v", f, rec.Header())
		}
		wantError(t, rec, http.StatusInternalServerError, "internal", "")
	}
}
