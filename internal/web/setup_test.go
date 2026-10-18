package web

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/setup"
)

const webSetup = `{"hookwitness": 1, "endpoints": [
  {"path": "/orders", "default": {"status": 202},
   "checks": [{"kind": "github", "secret": "GH", "enforce": true}, {"kind": "bearer", "secret": "TOK"}],
   "secrets": [{"name": "GH", "kind": "key"}, {"name": "TOK", "kind": "token"}]},
  {"path": "/refunds", "default": {"status": 200}}]}`

func TestExportSetupDownload(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, _ := e.st.CreateEndpoint(ctx, "/stripe/webhooks")
	c := emulate.Config{Default: emulate.Reply{Status: 200}}
	s, err := emulate.SealSecret("GH", emulate.SecretKey, "web_export_key_value", e.h.keys)
	if err != nil {
		t.Fatal(err)
	}
	c.Secrets = []emulate.Secret{s}
	e.st.SetConfig(ctx, a.ID, c.String())
	e.st.CreateEndpoint(ctx, "/b")

	rec := e.do("GET", fmt.Sprintf("/admin/e/%d/setup", a.ID), nil)
	if rec.Code != 200 || rec.Header().Get("Content-Disposition") != `attachment; filename="stripe-webhooks.hookwitness.json"` {
		t.Fatalf("export: %d %v", rec.Code, rec.Header())
	}
	for _, bad := range []string{"web_export_key_value", s.Sealed, s.Fingerprint} {
		if strings.Contains(rec.Body.String(), bad) {
			t.Fatalf("the export carries %q", bad)
		}
	}
	all := e.do("GET", "/admin/setup", nil)
	if f, err := setup.Parse(all.Body.Bytes(), setup.Options{Reserved: "/admin", MaxWait: e.h.maxWait}); err != nil || len(f.Endpoints) != 2 {
		t.Fatalf("export all: %v %s", err, all.Body)
	}
	if rec := e.do("GET", "/admin/e/unmatched/setup", nil); rec.Code != 404 {
		t.Fatalf("Unmatched: %d", rec.Code)
	}
	page := e.do("GET", fmt.Sprintf("/admin/e/%d/behaviour", a.ID), nil).Body.String()
	if !strings.Contains(page, fmt.Sprintf(`href="/admin/e/%d/setup" download>Export setup</a>`, a.ID)) {
		t.Fatal("the behaviour page has no Export setup")
	}
	if page := e.do("GET", "/admin/", nil).Body.String(); !strings.Contains(page, `href="/admin/setup" download>Export all</a>`) || !strings.Contains(page, `href="/admin/?edit=import">Import</a>`) {
		t.Fatal("the endpoints page lacks Export all or Import")
	}
}

func TestImportSetupDialog(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	page := e.do("GET", "/admin/?edit=import", nil).Body.String()
	if !strings.Contains(page, `<h2 id="dialog-title">Import endpoints</h2>`) || !strings.Contains(page, `name="file"`) {
		t.Fatalf("step 1:\n%s", page)
	}
	// A bad file: the first step again, saying why.
	rec := e.do("POST", "/admin/import", url.Values{"op": {"review"}, "text": {`{"hookwitness": 3, "endpoints": []}`}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Not imported: made by a newer hookwitness") {
		t.Fatalf("newer: %d %s", rec.Code, rec.Body)
	}
	rec = e.do("POST", "/admin/import", url.Values{"op": {"review"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "choose a setup file or paste its content") {
		t.Fatalf("empty: %d", rec.Code)
	}

	// /orders is taken.
	old, _ := e.st.CreateEndpoint(ctx, "/orders")
	rec = e.do("POST", "/admin/import", url.Values{"op": {"review"}, "text": {webSetup}})
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `name="path_0" value="/orders"`) || !strings.Contains(body, `name="replace_0"`) ||
		strings.Contains(body, `name="replace_1"`) || !strings.Contains(body, `name="secret_0_GH"`) || !strings.Contains(body, "/orders · default 202 · 2 checks") {
		t.Fatalf("review:\n%s", body)
	}
	if n, _ := e.st.Endpoints(ctx); len(n) != 1 {
		t.Fatal("the review imported something")
	}

	form := url.Values{"op": {"import"}, "text": {webSetup}, "path_0": {"/orders"}, "path_1": {"/refunds"}, "secret_0_GH": {"web_import_key_value"}}
	rec = e.do("POST", "/admin/import", form)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "/orders is already an endpoint: tick Replace") {
		t.Fatalf("taken, not ticked: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "web_import_key_value") {
		t.Fatal("a refused import shows the secret value")
	}
	if n, _ := e.st.Endpoints(ctx); len(n) != 1 {
		t.Fatal("a refused import created /refunds")
	}

	// Under another path instead; the value typed is sealed.
	form.Set("path_0", "/orders/v2")
	rec = e.do("POST", "/admin/import", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/admin/?") || !strings.Contains(loc, "unset=1") {
		t.Fatalf("redirect %s", loc)
	}
	id, cfg, _, err := e.st.EndpointByPath(ctx, "/orders/v2")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cfg, "web_import_key_value") {
		t.Fatal("the value was stored in plain text")
	}
	c, _ := emulate.Parse(cfg)
	if !c.Secrets[0].Set() || c.Secrets[1].Set() {
		t.Fatalf("secrets %+v", c.Secrets)
	}
	page = e.do("GET", loc, nil).Body.String()
	if !strings.Contains(page, "Imported: created /orders/v2, created /refunds.") || !strings.Contains(page, "1 secret has no value yet: the checks that use it give n/v") ||
		!strings.Contains(page, "1 secret not set</a>") {
		t.Fatalf("after the import:\n%s", page)
	}
	reg := e.do("GET", fmt.Sprintf("/admin/e/%d", id), nil).Body.String()
	if !strings.Contains(reg, "1 secret of this endpoint has no value yet") {
		t.Fatal("the register does not say a secret is not set")
	}
	beh := e.do("GET", fmt.Sprintf("/admin/e/%d/behaviour?tab=secrets", id), nil).Body.String()
	if !strings.Contains(beh, `<b class="unset">not set</b>`) || !strings.Contains(beh, ">Set value</a>") {
		t.Fatalf("secrets tab:\n%s", beh)
	}
	if chk := e.do("GET", fmt.Sprintf("/admin/e/%d/behaviour?tab=checks", id), nil).Body.String(); !strings.Contains(chk, "TOK not set: n/v</a>") {
		t.Fatal("the checks tab does not mark the check over an unset secret")
	}

	// Replace /orders (ticked): its id stays.
	form.Set("path_0", "/orders")
	form.Set("replace_0", "on")
	form.Set("path_1", "/refunds")
	form.Set("replace_1", "on")
	if rec := e.do("POST", "/admin/import", form); rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "replaced+%2Forders") {
		t.Fatalf("replace: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if got, _ := e.st.Endpoint(ctx, old.ID); !strings.Contains(got.Config, `"status":202`) {
		t.Fatalf("/orders not replaced: %s", got.Config)
	}
	// Two endpoints renamed onto one path.
	form.Set("path_1", "/orders")
	if rec := e.do("POST", "/admin/import", form); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "/orders is in the file twice") {
		t.Fatalf("same path twice: %d", rec.Code)
	}
}

func TestImportSetupFileUpload(t *testing.T) {
	e := newEnv(t)
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	mw.WriteField("op", "review")
	fw, _ := mw.CreateFormFile("file", "orders.hookwitness.json")
	fw.Write([]byte(webSetup))
	mw.Close()
	r := httptest.NewRequest("POST", "/admin/import", &b)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `name="path_1" value="/refunds"`) {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
}
