package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestEndpointOnAndOff(t *testing.T) {
	e := newEnv(t)
	a := e.endpoint(t, "/a")
	b := e.endpoint(t, "/b")
	if !a.Enabled {
		t.Fatalf("a new endpoint: %+v", a)
	}

	rec := e.do("PATCH", fmt.Sprintf("/endpoints/%d", a.ID), `{"enabled":false}`)
	got := decodeAs[endpointJSON](t, rec)
	if rec.Code != 200 || got.Enabled || got.Path != "/a" {
		t.Fatalf("turn off: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(e.log.String(), "endpoint turned off") {
		t.Fatalf("not logged:\n%s", e.log)
	}
	if got := decodeAs[endpointJSON](t, e.do("GET", fmt.Sprintf("/endpoints/%d", a.ID), "")); got.Enabled {
		t.Fatalf("read back: %+v", got)
	}

	list := func(q string) []string {
		t.Helper()
		rec := e.do("GET", "/endpoints"+q, "")
		if rec.Code != 200 {
			t.Fatalf("list %s: %d %s", q, rec.Code, rec.Body)
		}
		var paths []string
		for _, ep := range decodeAs[struct{ Endpoints []endpointJSON }](t, rec).Endpoints {
			paths = append(paths, fmt.Sprintf("%s:%v", ep.Path, ep.Enabled))
		}
		return paths
	}
	if got := fmt.Sprint(list("")); got != "[/a:false /b:true]" {
		t.Fatalf("all: %s", got)
	}
	if got := fmt.Sprint(list("?enabled=false")); got != "[/a:false]" {
		t.Fatalf("off: %s", got)
	}
	if got := fmt.Sprint(list("?enabled=true")); got != "[/b:true]" {
		t.Fatalf("on: %s", got)
	}
	wantError(t, e.do("GET", "/endpoints?enabled=maybe", ""), http.StatusBadRequest, "invalid", "enabled")

	// Path and state in one call; a taken path changes neither.
	wantError(t, e.do("PATCH", fmt.Sprintf("/endpoints/%d", b.ID), `{"path":"/a","enabled":false}`), http.StatusConflict, "duplicate_path", "path")
	if got := decodeAs[endpointJSON](t, e.do("GET", fmt.Sprintf("/endpoints/%d", b.ID), "")); !got.Enabled || got.Path != "/b" {
		t.Fatalf("after a refused change: %+v", got)
	}
	rec = e.do("PATCH", fmt.Sprintf("/endpoints/%d", a.ID), `{"path":"/a2","enabled":true}`)
	if got := decodeAs[endpointJSON](t, rec); rec.Code != 200 || !got.Enabled || got.Path != "/a2" {
		t.Fatalf("move and turn on: %d %s", rec.Code, rec.Body)
	}
	wantError(t, e.do("PATCH", fmt.Sprintf("/endpoints/%d", a.ID), `{"enabled":"no"}`), http.StatusBadRequest, "bad_json", "enabled")
	wantError(t, e.do("PATCH", fmt.Sprintf("/endpoints/%d", a.ID), `{"enabled":null}`), http.StatusUnprocessableEntity, "invalid", "path")
}
