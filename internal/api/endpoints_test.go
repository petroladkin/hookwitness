package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/catch"
)

func TestCreateEndpoint(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/endpoints", `{"path":"/github/hooks"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	ep := decodeAs[endpointJSON](t, rec)
	if ep.ID < 1 || ep.Path != "/github/hooks" || ep.URL != "http://example.com/github/hooks" || ep.Requests != 0 || time.Time(ep.Created).IsZero() {
		t.Fatalf("endpoint %+v", ep)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/api/v1/endpoints/1" {
		t.Fatalf("Location %q", loc)
	}

	// No path, or an empty one: a random path, as in the UI.
	for _, body := range []string{"", `{}`, `{"path":"  "}`} {
		rec := e.call(t.Context(), "POST", "/endpoints", body, "Content-Type", "application/json")
		if rec.Code != http.StatusCreated {
			t.Fatalf("random path from %q: %d %s", body, rec.Code, rec.Body)
		}
		if p := decodeAs[endpointJSON](t, rec).Path; len(p) != 13 || !strings.HasPrefix(p, "/") {
			t.Fatalf("random path %q", p)
		}
	}
}

func TestCreateEndpointRefused(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t, "/taken")
	wantError(t, e.do("POST", "/endpoints", `{"path":"/taken"}`), http.StatusConflict, "duplicate_path", "path")
	for _, p := range []string{"/healthz", "/admin", "/admin/api/v1/x", "no-slash", "/a//b", "/a/../b"} {
		got := wantError(t, e.do("POST", "/endpoints", `{"path":"`+p+`"}`), http.StatusUnprocessableEntity, "invalid", "path")
		// The same message as the UI gives.
		if want := catch.CheckPath("/admin", p); got.Error.Message != want {
			t.Fatalf("%s: message %q, want %q", p, got.Error.Message, want)
		}
	}
}

func TestListAndFindEndpoints(t *testing.T) {
	e := newEnv(t)
	a := e.endpoint(t, "/b/hooks")
	e.endpoint(t, "/a/hooks")
	e.add(t, a.ID, `{}`, nil)

	type list struct{ Endpoints []endpointJSON }
	all := decodeAs[list](t, e.do("GET", "/endpoints", ""))
	if len(all.Endpoints) != 2 || all.Endpoints[0].Path != "/a/hooks" || all.Endpoints[1].Requests != 1 || all.Endpoints[1].Last == nil {
		t.Fatalf("list %+v", all)
	}
	found := decodeAs[list](t, e.do("GET", "/endpoints?path=/b/hooks", ""))
	if len(found.Endpoints) != 1 || found.Endpoints[0].ID != a.ID {
		t.Fatalf("by path %+v", found)
	}
	none := e.do("GET", "/endpoints?path=/missing", "")
	if strings.TrimSpace(none.Body.String()) != "{\n  \"endpoints\": []\n}" {
		t.Fatalf("missing path: %s", none.Body)
	}

	one := decodeAs[endpointJSON](t, e.do("GET", "/endpoints/1", ""))
	if one.ID != a.ID || one.Path != "/b/hooks" {
		t.Fatalf("get %+v", one)
	}
	wantError(t, e.do("GET", "/endpoints/99", ""), http.StatusNotFound, "not_found", "")
	wantError(t, e.do("GET", "/endpoints/0", ""), http.StatusNotFound, "not_found", "")
	wantError(t, e.do("GET", "/endpoints/abc", ""), http.StatusNotFound, "not_found", "")
}
