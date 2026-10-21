package web

import (
	"context"
	"github.com/petroladkin/hookwitness/internal/catch"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/petroladkin/hookwitness/internal/store"
)

func TestSchemaDraftFlow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, _ := e.st.CreateEndpoint(ctx, "/shop/events")
	base := "/admin/e/" + id(ep.ID)
	e.edit(ep.ID, url.Values{"op": {"schema-save"}, "i": {"new"}, "name": {"OrderPaid"}, "text": {orderPaid}})
	e.edit(ep.ID, url.Values{"op": {"check-save"}, "i": {"new"}, "kind": {"schema"}, "select_from": {"body"}, "select_key": {"/type"},
		"select_map": {"order.paid = OrderPaid"}})
	e.add(t, ep.ID, `{"type":"order.shipped","order_id":1,"at":"2026-10-12T17:28:07Z"}`, nil) // 1
	e.add(t, ep.ID, `{"type":"order.shipped","order_id":2}`, nil)                             // 2
	paid := e.add(t, ep.ID, `{"type":"order.paid","order_id":3,"amount":{"value":5},"coupon":"X"}`, nil)
	e.add(t, ep.ID, `<xml/>`, nil)                                                                // 4
	e.add(t, ep.ID, "", func(r *store.Request) { r.Body, r.TooLarge, r.Size = nil, true, 1<<30 }) // 5
	page := func(target string) string {
		t.Helper()
		rec := e.do("GET", target, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", target, rec.Code)
		}
		return html.UnescapeString(rec.Body.String())
	}

	// The buttons: on the register and the exhibit; the dialog offers the
	// discriminator's values, most frequent first.
	reg := page(base)
	if !strings.Contains(reg, `href="`+base+`?draft"`) {
		t.Fatal("no Draft schema on the register")
	}
	ex := page(base + "/r/" + id(paid.ID) + "?draft")
	if !strings.Contains(ex, `name="pick" value="3"`) || !strings.Contains(ex, `<option value="order.shipped">order.shipped · 2</option>`) ||
		!strings.Contains(ex, `<option value="pick" selected>`) {
		t.Fatalf("dialog from the exhibit:\n%s", between(ex, "<dialog", "</dialog>"))
	}
	if !strings.Contains(page(base+"?draft"), `<option value="value" selected>all requests with the same /type</option>`) {
		t.Fatal("the dialog from the register starts with the values")
	}
	if strings.Contains(page("/admin/e/unmatched"), "Draft schema") {
		t.Fatal("Unmatched offers a draft")
	}

	// All with order.shipped: a new schema, offered to the mapping.
	ed := page(base + "/behaviour?tab=schemas&edit=schema-draft&by=value&value=order.shipped&pick=")
	for _, want := range []string{`name="name" value="OrderShipped"`, `Drafted from the 2 requests with /type = order.shipped.`,
		`"required": ["type", "order_id"]`, `"format": "date-time"`, `name="i" value="new"`,
		`<input type="checkbox" name="map" checked> Also map <code>order.shipped</code> to this schema in check 1 (JSON Schema by /type)`} {
		if !strings.Contains(ed, want) {
			t.Errorf("editor lacks %s\n%s", want, between(ed, "<dialog", "</dialog>"))
		}
	}
	if len(e.config(t, ep.ID).Schemas) != 1 {
		t.Fatal("a draft was stored before saving")
	}
	text := between(ed, `name="text" rows="14" spellcheck="false" placeholder="{"type": "object"}">`, "</textarea>")
	rec := e.edit(ep.ID, url.Values{"op": {"schema-save"}, "i": {"new"}, "name": {"OrderShipped"}, "text": {text},
		"draft_from": {"x"}, "map": {"on"}, "map_check": {"1"}, "map_value": {"order.shipped"}, "map_label": {"JSON Schema by /type"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rec.Code, errorOf(rec.Body.String()))
	}
	cfg := e.config(t, ep.ID)
	if len(cfg.Schemas) != 2 || cfg.Checks[0].Select.Map[1].Value != "order.shipped" || cfg.Checks[0].Select.Map[1].Schema != "OrderShipped" {
		t.Fatalf("saved %+v %+v", cfg.Schemas, cfg.Checks[0].Select.Map)
	}

	// Re-drafting a mapped value: the stored schema, its changes, no mapping offer.
	ed = page(base + "/behaviour?tab=schemas&edit=schema-draft&by=pick&pick=3")
	for _, want := range []string{`name="name" value="OrderPaid"`, `name="i" value="0"`, "Against the stored OrderPaid · saving replaces it",
		"/coupon: new key, required", "/type: new key, required", "/amount/value: now required", "Drafted from request 3."} {
		if !strings.Contains(ed, want) {
			t.Errorf("re-draft lacks %s", want)
		}
	}
	if strings.Contains(ed, `name="map"`) {
		t.Error("a mapped value is offered again")
	}

	// No draft, and why.
	for q, why := range map[string]string{
		"pick=4":           "No draft: request 4: not JSON",
		"pick=5":           "No draft: request 5: its body was over the limit and was not kept.",
		"pick=9":           "No draft: some of those numbers are not kept requests of this endpoint.",
		"pick=":            "No draft: give the request numbers",
		"pick=x":           `No draft: "x" is not a request number`,
		"pick=1-101":       "No draft: at most 100 requests can be picked.",
		"by=value&value=z": "No draft: no kept request has /type = z.",
	} {
		if p := page(base + "/behaviour?tab=schemas&edit=schema-draft&" + q); !strings.Contains(p, why) || strings.Contains(p, "<dialog") {
			t.Errorf("%s: want %q", q, why)
		}
	}
}

// A draft holds at most catch.MaxDraftBytes of bodies: picked requests
// past it are refused; all with a value keep the newest that fit.
func TestSchemaDraftBytes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, _ := e.st.CreateEndpoint(ctx, "/shop/events")
	base := "/admin/e/" + id(ep.ID)
	e.edit(ep.ID, url.Values{"op": {"schema-save"}, "i": {"new"}, "name": {"OrderPaid"}, "text": {orderPaid}})
	e.edit(ep.ID, url.Values{"op": {"check-save"}, "i": {"new"}, "kind": {"schema"}, "select_from": {"body"}, "select_key": {"/type"},
		"select_map": {"order.paid = OrderPaid"}})
	for i := range 4 {
		e.add(t, ep.ID, `{"type":"order.shipped","order_id":`+strconv.Itoa(i)+`,"pad":"`+strings.Repeat("x", 1000)+`"}`, nil)
	}
	old := catch.MaxDraftBytes
	catch.MaxDraftBytes = 2500
	t.Cleanup(func() { catch.MaxDraftBytes = old })
	get := func(q string) string {
		return html.UnescapeString(e.do("GET", base+"/behaviour?tab=schemas&edit=schema-draft&"+q, nil).Body.String())
	}
	if p := get("pick=1-4"); !strings.Contains(p, "No draft: at most 2500 bytes of bodies at a time.") {
		t.Fatalf("picked past the budget:\n%s", p)
	}
	if p := get("by=value&value=order.shipped"); !strings.Contains(p, "Drafted from the 2 newest of 4 requests with /type = order.shipped.") {
		t.Fatalf("by value past the budget:\n%s", between(p, "<dialog", "</dialog>"))
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	if j := strings.Index(s, b); j >= 0 {
		return s[:j]
	}
	return s
}
