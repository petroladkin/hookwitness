package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// apiCall sends a JSON request to the API as mounted by the UI.
func (e env) apiCall(method, target, body string, host string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if host != "" {
		r.Host = host
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct{ Error struct{ Code string } }
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("not a JSON error: %s", rec.Body)
	}
	return b.Error.Code
}

// TestAPIMounted: the API answers under the admin prefix with the UI's
// host check and cross-origin protection, its errors in JSON.
func TestAPIMounted(t *testing.T) {
	e := newEnv(t)
	if rec := e.apiCall("POST", "/admin/api/v1/endpoints", `{"path":"/hooks"}`, "127.0.0.1:8080"); rec.Code != http.StatusCreated {
		t.Fatalf("create through the UI's handler: %d %s", rec.Code, rec.Body)
	}
	if rec := e.apiCall("GET", "/admin/api/v1/endpoints", "", "hooks.example.test:8080"); rec.Code != http.StatusOK {
		t.Fatalf("an -admin-hosts name: %d %s", rec.Code, rec.Body)
	}
	rec := e.apiCall("GET", "/admin/api/v1/endpoints", "", "host.docker.internal:8080")
	if rec.Code != http.StatusMisdirectedRequest || errorCode(t, rec) != "wrong_host" {
		t.Fatalf("an unknown host name: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "HOOKWITNESS_ADMIN_HOSTS") {
		t.Fatalf("the refusal does not say how to allow the name: %s", rec.Body)
	}
	rec = e.apiCall("POST", "/admin/api/v1/endpoints", `{"path":"/x"}`, "", "Sec-Fetch-Site", "cross-site")
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != "cross_origin" {
		t.Fatalf("a cross-site write: %d %s", rec.Code, rec.Body)
	}
	rec = e.apiCall("PUT", "/admin/api/v1/endpoints/1/secrets/T", `{"kind":"token","value":"v"}`, "", "Origin", "https://attacker.example")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a write from another origin: %d %s", rec.Code, rec.Body)
	}
	// The UI keeps its own answers.
	if rec := e.do("GET", "/admin/", nil); rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("UI: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

// TestAPIWrittenBehaviourShowsInTheUI: a check and a secret set through the
// API are what the behaviour page shows and what the catcher would use.
func TestAPIWrittenBehaviourShowsInTheUI(t *testing.T) {
	e := newEnv(t)
	e.apiCall("POST", "/admin/api/v1/endpoints", `{"path":"/hooks"}`, "")
	if rec := e.apiCall("PUT", "/admin/api/v1/endpoints/1/secrets/PASS", `{"kind":"token","value":"pw-123"}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("secret: %d %s", rec.Code, rec.Body)
	}
	tag := e.apiCall("GET", "/admin/api/v1/endpoints/1/behaviour", "", "").Header().Get("ETag")
	rec := e.apiCall("PUT", "/admin/api/v1/endpoints/1/behaviour",
		`{"checks":[{"kind":"basic","secret":"PASS","user":"sender","enforce":true}],"rules":[{"when":{"kind":"first","n":2},"do":"reply","reply":{"status":503}}]}`,
		"", "If-Match", tag)
	if rec.Code != http.StatusOK {
		t.Fatalf("behaviour: %d %s", rec.Code, rec.Body)
	}
	page := e.do("GET", "/admin/e/1/behaviour?tab=checks", nil).Body.String()
	for _, want := range []string{"Basic credentials", "secret PASS · user sender · enforced · else 401"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the behaviour page does not show %q", want)
		}
	}
	if page := e.do("GET", "/admin/e/1/behaviour?tab=rules", nil).Body.String(); !strings.Contains(page, "first 2") {
		t.Fatal("the behaviour page does not show the rule")
	}
	// The UI edits on top of what the API wrote (same version scheme).
	cfg := e.config(t, 1)
	if len(cfg.Checks) != 1 || len(cfg.Secrets) != 1 || cfg.Secrets[0].Hash == "" {
		t.Fatalf("stored %+v", cfg)
	}
}
