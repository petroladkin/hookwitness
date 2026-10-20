package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

func TestSwitchEndpoint(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, _ := e.st.CreateEndpoint(ctx, "/orders")
	e.st.CreateEndpoint(ctx, "/refunds")
	reg := fmt.Sprintf("/admin/e/%d", a.ID)

	// The Endpoints page: a switch per row, all on.
	page := e.do("GET", "/admin/", nil).Body.String()
	if strings.Count(page, `role="switch" aria-checked="true"`) != 2 {
		t.Fatalf("all on:\n%s", page)
	}

	rec := e.do("POST", reg+"/switch", url.Values{"enabled": {"false"}, "next": {"/admin/"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/" {
		t.Fatalf("turn off: %d %v", rec.Code, rec.Header())
	}
	if ep, _ := e.st.Endpoint(ctx, a.ID); ep.Enabled {
		t.Fatal("still on")
	}

	page = e.do("GET", "/admin/", nil).Body.String()
	if !strings.Contains(page, `<tr class="off">`) || strings.Count(page, `aria-checked="false"`) != 1 {
		t.Fatalf("the off row:\n%s", page)
	}

	// The register's picker groups them, the off one marked.
	page = e.do("GET", reg, nil).Body.String()
	for _, want := range []string{`<span class="menu-label">On</span>`, `<span class="menu-label">Off</span>`, `<a class="menu-item off" href="/admin/e/1" aria-current="page"><span>/orders</span>`} {
		if !strings.Contains(page, want) {
			t.Fatalf("picker lacks %s:\n%s", want, page)
		}
	}
	if strings.Index(page, `<span>/refunds</span>`) > strings.Index(page, `<span class="menu-label">Off</span>`) {
		t.Fatal("the on endpoint is not in the On group")
	}
	if strings.Contains(page, `class="menu-none"`) {
		t.Fatal("None under a group that has an endpoint")
	}

	// With every endpoint off, the empty On group says None.
	e.do("POST", "/admin/e/2/switch", url.Values{"enabled": {"false"}})
	page = e.do("GET", reg, nil).Body.String()
	if on, none, off := strings.Index(page, `<span class="menu-label">On</span>`), strings.Index(page, `<span class="menu-none">None</span>`), strings.Index(page, `<span class="menu-label">Off</span>`); on < 0 || none < on || off < none {
		t.Fatalf("empty On group:\n%s", page)
	}
	e.do("POST", "/admin/e/2/switch", url.Values{"enabled": {"true"}})

	// The register page says so, with the switch.
	page = e.do("GET", reg, nil).Body.String()
	if !strings.Contains(page, "Off: requests to this path are answered 404") || !strings.Contains(page, `aria-checked="false"`) {
		t.Fatalf("register while off:\n%s", page)
	}

	rec = e.do("POST", reg+"/switch", url.Values{"enabled": {"true"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != reg {
		t.Fatalf("turn on: %d %v", rec.Code, rec.Header())
	}
	page = e.do("GET", reg, nil).Body.String()
	if strings.Contains(page, "Off: requests") {
		t.Fatal("the banner stayed")
	}

	if rec := e.do("POST", reg+"/switch", url.Values{"enabled": {"maybe"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad value: %d", rec.Code)
	}
	if rec := e.do("POST", "/admin/e/unmatched/switch", url.Values{"enabled": {"false"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("Unmatched: %d", rec.Code)
	}
	if rec := e.do("POST", "/admin/e/99/switch", url.Values{"enabled": {"false"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", rec.Code)
	}
}

// An Unmatched row for an off endpoint's path says so.
func TestUnmatchedRowOfAnOffEndpoint(t *testing.T) {
	e := newEnv(t)
	out := emulate.Outcome{Source: emulate.FromOff, Action: emulate.ActReply, Status: 404}
	r := e.add(t, store.Unmatched, "{}", func(r *store.Request) { r.Path, r.Status, r.Outcome = "/orders", 404, out.String() })
	page := e.do("GET", "/admin/e/unmatched", nil).Body.String()
	if !strings.Contains(page, "endpoint off</span>") {
		t.Fatalf("row:\n%s", page)
	}
	page = e.do("GET", fmt.Sprintf("/admin/e/unmatched/r/%d", r.ID), nil).Body.String()
	if !strings.Contains(page, "the endpoint at this path is off") {
		t.Fatalf("exhibit:\n%s", page)
	}
}
