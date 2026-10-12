package web

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/petroladkin/hookwitness/internal/store"
)

const orderPaid = `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["order_id","amount"],
 "properties":{"order_id":{"type":"integer"},"amount":{"type":"object","properties":{"value":{"type":"integer"}}}}}`

// upload posts the schema editor as a browser does with a file chosen.
func (e env) upload(t *testing.T, endpoint int64, name, filename, content string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	ep, _ := e.st.Endpoint(context.Background(), endpoint)
	for k, v := range map[string]string{"op": "schema-save", "i": "new", "name": name, "text": "", "v": configVersion(ep.Config)} {
		w.WriteField(k, v)
	}
	fw, _ := w.CreateFormFile("file", filename)
	fw.Write([]byte(content))
	w.Close()
	r := httptest.NewRequest("POST", "/admin/e/"+id(endpoint)+"/behaviour", &buf)
	r.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	return rec
}

func TestSchemaEditing(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/orders")
	if rec := e.edit(ep.ID, url.Values{"op": {"schema-save"}, "i": {"new"}, "name": {"OrderPaid"}, "text": {orderPaid}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("paste: %d %s", rec.Code, errorOf(rec.Body.String()))
	}
	if rec := e.upload(t, ep.ID, "Address", "address.json", `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object"}`); rec.Code != http.StatusSeeOther {
		t.Fatalf("upload: %d %s", rec.Code, errorOf(rec.Body.String()))
	}
	cfg := e.config(t, ep.ID)
	if len(cfg.Schemas) != 2 || cfg.Schemas[1].Name != "Address" || !strings.Contains(cfg.Schemas[1].Text, "draft-07") {
		t.Fatalf("schemas %+v", cfg.Schemas)
	}
	page := e.do("GET", "/admin/e/"+id(ep.ID)+"/behaviour?tab=schemas", nil).Body.String()
	for _, want := range []string{"<b>OrderPaid</b> · 2020-12", "<b>Address</b> · draft-07", "not used:"} {
		if !strings.Contains(page, want) {
			t.Fatalf("schemas tab lacks %q", want)
		}
	}
	if !strings.Contains(e.do("GET", "/admin/e/"+id(ep.ID)+"/behaviour?tab=schemas&edit=schema-new", nil).Body.String(), `enctype="multipart/form-data"`) {
		t.Fatal("the schema editor cannot upload a file")
	}

	// Refused at save, with the reason, and nothing stored.
	for text, want := range map[string]string{
		`{"type": 5}`: "not a valid schema",
		`{"$ref": "https://example.com/order.json"}`: "never fetched",
		`{"type": "object",}`:                        "is not valid JSON",
		``:                                           "paste the schema or choose a file",
	} {
		rec := e.edit(ep.ID, url.Values{"op": {"schema-save"}, "i": {"new"}, "name": {"Bad"}, "text": {text}})
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(errorOf(rec.Body.String()), want) {
			t.Fatalf("%q: %d %q, want %q", text, rec.Code, errorOf(rec.Body.String()), want)
		}
	}
	if n := len(e.config(t, ep.ID).Schemas); n != 2 {
		t.Fatalf("%d schemas after refusals", n)
	}

	// A check picking the schema by /type.
	rec := e.edit(ep.ID, url.Values{"op": {"check-save"}, "i": {"new"}, "kind": {"schema"}, "select_from": {"body"}, "select_key": {"/type"},
		"select_map": {"order.paid = OrderPaid\nshipping.address = Address\n"}, "enforce": {"on"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("check: %d %s", rec.Code, errorOf(rec.Body.String()))
	}
	ch := e.config(t, ep.ID).Checks[0]
	if ch.Select == nil || ch.Select.Key != "/type" || len(ch.Select.Map) != 2 || ch.Select.Map[1].Schema != "Address" || !ch.Enforce {
		t.Fatalf("check %+v", ch)
	}
	checks := e.do("GET", "/admin/e/"+id(ep.ID)+"/behaviour?tab=checks", nil).Body.String()
	if !strings.Contains(checks, `JSON Schema by /type</b> · &#34;order.paid&#34; → OrderPaid, &#34;shipping.address&#34; → Address · other values not checked · enforced · else 400 listing the problems`) {
		t.Fatal("check summary missing")
	}
	// A used schema can be neither deleted nor renamed.
	if rec := e.edit(ep.ID, url.Values{"op": {"schema-del"}, "i": {"0"}}); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(errorOf(rec.Body.String()), "used by check 1") {
		t.Fatalf("delete used: %d %s", rec.Code, errorOf(rec.Body.String()))
	}
	if rec := e.edit(ep.ID, url.Values{"op": {"schema-save"}, "i": {"0"}, "name": {"Paid"}, "text": {orderPaid}}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("rename used: %d", rec.Code)
	}
	if rec := e.edit(ep.ID, url.Values{"op": {"check-save"}, "i": {"new"}, "kind": {"schema"}, "select_from": {"body"}, "select_key": {"/t"}, "select_map": {"no equals sign"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad mapping line: %d", rec.Code)
	}
}

func TestSchemaVerdictOnTheExhibit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, _ := e.st.CreateEndpoint(ctx, "/orders")
	e.edit(ep.ID, url.Values{"op": {"schema-save"}, "i": {"new"}, "name": {"OrderPaid"}, "text": {orderPaid}})
	e.edit(ep.ID, url.Values{"op": {"check-save"}, "i": {"new"}, "kind": {"schema"}, "schema": {"OrderPaid"}})
	r := e.add(t, ep.ID, `{"order_id":"42","amount":{"value":1.5}}`, nil)
	base := "/admin/e/" + id(ep.ID)
	e.do("POST", base+"/recheck", url.Values{}) // the verdicts of stored requests come from a re-check here

	page := e.do("GET", base+"/r/"+id(r.ID), nil).Body.String()
	for _, want := range []string{
		`<div class="vstamp">Schema: fail, 2 errors</div>`,
		`<tr><th>/order_id</th><td><code>type</code>`,
		`<tr><th>/amount/value</th><td><code>type</code>`,
		`<span class="s bad">&#34;42&#34;</span>`,
		`<span class="k bad">&#34;order_id&#34;</span>`,
		`<span class="n bad">1.5</span>`,
		"Marked in the body",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("exhibit lacks %q", want)
		}
	}
	if strings.Contains(page, `<span class="k bad">&#34;amount&#34;</span>`) {
		t.Fatal("a member without a problem is marked")
	}
	if !strings.Contains(e.do("GET", base, nil).Body.String(), `<b class="v-fail">1 fail</b>`) {
		t.Fatal("register counts")
	}

	// After the schema is fixed, one re-check turns the verdict.
	e.edit(ep.ID, url.Values{"op": {"schema-save"}, "i": {"0"}, "name": {"OrderPaid"}, "text": {`{"type":"object"}`}})
	e.do("POST", base+"/recheck", url.Values{})
	got, _ := e.st.Request(ctx, r.ID)
	if got.Verdict != "pass" {
		t.Fatalf("after the schema edit and a re-check: %q %s", got.Verdict, got.Verdicts)
	}
	tooBig := e.add(t, ep.ID, "", func(r *store.Request) { r.TooLarge, r.Size, r.Status = true, 1<<30, 413 })
	e.do("POST", base+"/recheck", url.Values{})
	if got, _ := e.st.Request(ctx, tooBig.ID); got.Verdict != "n/v" {
		t.Fatalf("a body not kept: %q", got.Verdict)
	}
}

func TestSchemaNotCheckedCount(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/orders")
	e.edit(ep.ID, url.Values{"op": {"schema-save"}, "i": {"new"}, "name": {"OrderPaid"}, "text": {orderPaid}})
	e.edit(ep.ID, url.Values{"op": {"check-save"}, "i": {"new"}, "kind": {"schema"}, "select_from": {"header"}, "select_key": {"X-Event"}, "select_map": {"order.paid = OrderPaid"}})
	r := e.add(t, ep.ID, `{}`, nil)
	e.do("POST", "/admin/e/"+id(ep.ID)+"/recheck", url.Values{})
	page := e.do("GET", "/admin/e/"+id(ep.ID)+"/r/"+id(r.ID), nil).Body.String()
	if !strings.Contains(page, `<b class="v-nv">1 n/c</b>`) || !strings.Contains(page, "Schema: not checked") || !strings.Contains(page, "no header X-Event in the request") {
		t.Fatal("n/c not shown")
	}
}
