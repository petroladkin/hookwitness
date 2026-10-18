package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/petroladkin/hookwitness/internal/setup"
)

const (
	testKey   = "gh_signing_key_test_value"
	testToken = "bearer_token_test_value"
)

// configured makes /orders with checks, rules, vars, a schema and two
// secrets with values.
func (e env) configured(t *testing.T) endpointJSON {
	t.Helper()
	ep := e.endpoint(t, "/orders")
	for _, sec := range [][2]string{ // in order: secrets keep the order they were added in
		{"GH", `{"kind":"key","value":"` + testKey + `"}`},
		{"TOK", `{"kind":"token","value":"` + testToken + `"}`},
	} {
		name, body := sec[0], sec[1]
		if rec := e.do("PUT", fmt.Sprintf("/endpoints/%d/secrets/%s", ep.ID, name), body); rec.Code != 200 {
			t.Fatalf("secret %s: %d %s", name, rec.Code, rec.Body)
		}
	}
	_, tag := e.behaviour(t, ep.ID)
	rec := e.putBehaviour(ep.ID, tag, `{
		"default": {"status": 202, "headers": [{"name": "X-Env", "value": "{{.Var \"ENV\"}}"}], "body": "ok"},
		"rules": [{"when": {"kind": "every", "n": 3}, "do": "reply", "reply": {"status": 503, "body": "later"}}],
		"checks": [{"kind": "github", "secret": "GH", "enforce": true}, {"kind": "bearer", "secret": "TOK"}, {"kind": "schema", "schema": "Order"}],
		"vars": [{"name": "ENV", "value": "staging"}],
		"schemas": [{"name": "Order", "text": "{\"type\":\"object\"}"}]}`)
	if rec.Code != 200 {
		t.Fatalf("behaviour: %d %s", rec.Code, rec.Body)
	}
	return ep
}

func TestExportSetup(t *testing.T) {
	e := newEnv(t)
	ep := e.configured(t)
	e.endpoint(t, "/refunds")

	rec := e.do("GET", fmt.Sprintf("/setup?id=%d", ep.ID), "")
	if rec.Code != 200 {
		t.Fatalf("export: %d %s", rec.Code, rec.Body)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="orders.hookwitness.json"` {
		t.Errorf("Content-Disposition %q", cd)
	}
	body := rec.Body.String()
	stored := e.stored(t, ep.ID)
	derived := []string{testKey, testToken}
	for _, s := range stored.Secrets {
		derived = append(derived, s.Sealed, s.Hash, s.Fingerprint)
	}
	for _, bad := range derived {
		if bad != "" && strings.Contains(body, bad) {
			t.Errorf("the export contains %q", bad)
		}
	}
	for _, bad := range []string{"fingerprint", "sealed", "hash", "created_at", "counter", "enabled", `"id"`} {
		if strings.Contains(body, bad) {
			t.Errorf("the export has %s:\n%s", bad, body)
		}
	}
	f, err := setup.Parse(rec.Body.Bytes(), setup.Options{Reserved: "/admin", MaxWait: e.h.o.MaxWait})
	if err != nil || len(f.Endpoints) != 1 || len(f.Endpoints[0].Secrets) != 2 {
		t.Fatalf("export does not parse: %v\n%s", err, body)
	}

	all := e.do("GET", "/setup", "")
	if got := decodeAs[setup.File](t, all); fmt.Sprint(got.Paths()) != "[/orders /refunds]" {
		t.Fatalf("all: %s", all.Body)
	}
	if cd := all.Header().Get("Content-Disposition"); cd != `attachment; filename="endpoints.hookwitness.json"` {
		t.Errorf("Content-Disposition %q", cd)
	}
	wantError(t, e.do("GET", "/setup?id=99", ""), http.StatusNotFound, "not_found", "id")
	wantError(t, e.do("GET", "/setup?id=x", ""), http.StatusBadRequest, "invalid", "id")
}

func TestImportSetup(t *testing.T) {
	src := newEnv(t)
	ep := src.configured(t)
	file := src.do("GET", fmt.Sprintf("/setup?id=%d", ep.ID), "").Body.String()
	exported, _ := src.behaviour(t, ep.ID)

	// Another instance, another secret key.
	e := newEnv(t)
	other := e.endpoint(t, "/other")
	rec := e.do("POST", "/setup", file)
	if rec.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	res := decodeAs[struct{ Endpoints []setup.Result }](t, rec).Endpoints
	if len(res) != 1 || res[0].Path != "/orders" || res[0].Result != setup.Created || fmt.Sprint(res[0].Unset) != "[GH TOK]" {
		t.Fatalf("result %s", rec.Body)
	}
	if !strings.Contains(e.log.String(), "endpoints imported") || !strings.Contains(e.log.String(), "/orders") {
		t.Errorf("not logged:\n%s", e.log)
	}
	imported, _ := e.behaviour(t, res[0].ID)
	for i, s := range imported.Secrets {
		if s.Set || s.Fingerprint != "" || s.Name != exported.Secrets[i].Name || s.Kind != exported.Secrets[i].Kind {
			t.Errorf("imported secret %+v", s)
		}
	}
	if !exported.Secrets[0].Set {
		t.Error("a secret with a value reads as not set")
	}
	strip := func(b behaviourJSON) behaviourJSON { b.Secrets = nil; return b }
	if !reflect.DeepEqual(strip(imported), strip(exported)) {
		t.Fatalf("behaviour differs:\n%+v\n%+v", imported, exported)
	}

	// Taken: 409 naming the path; a new path in the same file is not created.
	two := strings.Replace(file, `"endpoints": [`, `"endpoints": [{"path": "/fresh", "default": {"status": 200}},`, 1)
	got := wantError(t, e.do("POST", "/setup", two), http.StatusConflict, "duplicate_path", "")
	if !strings.Contains(got.Error.Message, "/orders") || !strings.Contains(got.Error.Message, "on_conflict=replace") {
		t.Errorf("message %q", got.Error.Message)
	}
	if rec := e.do("GET", "/endpoints?path=/fresh", ""); strings.Contains(rec.Body.String(), "/fresh") {
		t.Fatal("/fresh was created by a refused import")
	}

	// A value given afterwards survives a repeated replace.
	if rec := e.do("PUT", fmt.Sprintf("/endpoints/%d/secrets/GH", res[0].ID), `{"value":"`+testKey+`"}`); rec.Code != 200 {
		t.Fatalf("set GH: %d %s", rec.Code, rec.Body)
	}
	before, _ := e.behaviour(t, res[0].ID)
	rec = e.do("POST", "/setup?on_conflict=replace", file)
	res = decodeAs[struct{ Endpoints []setup.Result }](t, rec).Endpoints
	if rec.Code != 200 || res[0].Result != setup.Unchanged || fmt.Sprint(res[0].Unset) != "[TOK]" {
		t.Fatalf("replace: %d %s", rec.Code, rec.Body)
	}
	after, _ := e.behaviour(t, res[0].ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("a repeated import changed the behaviour")
	}
	if b, _ := e.behaviour(t, other.ID); len(b.Checks) != 0 {
		t.Fatal("another endpoint changed")
	}

	wantError(t, e.do("POST", "/setup?on_conflict=merge", file), http.StatusBadRequest, "invalid", "on_conflict")
	for name, body := range map[string]string{
		"not a setup": `{"endpoints":[]}`,
		"newer":       strings.Replace(file, `"hookwitness": 1`, `"hookwitness": 7`, 1),
		"unknown":     strings.Replace(file, `"path": "/orders"`, `"path": "/orders", "color": 1`, 1),
		"bad path":    strings.Replace(file, `"path": "/orders"`, `"path": "/admin/orders"`, 1),
	} {
		rec := e.do("POST", "/setup", body)
		if rec.Code != http.StatusUnprocessableEntity || decodeAs[apiErr](t, rec).Error.Code != "invalid_setup" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	big := `{"hookwitness":1,"endpoints":[{"path":"/x","default":{"status":200,"body":"` + strings.Repeat("a", setup.MaxSize) + `"}}]}`
	wantError(t, e.do("POST", "/setup", big), http.StatusRequestEntityTooLarge, "too_large", "")
}

// An imported secret reads as not set in the behaviour.
func TestImportedSecretReadsNotSet(t *testing.T) {
	e := newEnv(t)
	file := `{"hookwitness":1,"endpoints":[{"path":"/gh","default":{"status":200},
		"checks":[{"kind":"github","secret":"GH","enforce":true}],"secrets":[{"name":"GH","kind":"key"}]}]}`
	if rec := e.do("POST", "/setup", file); rec.Code != 201 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	b, _ := e.behaviour(t, 1)
	var raw map[string]any
	json.Unmarshal(mustJSON(t, b.Secrets[0]), &raw)
	if raw["set"] != false {
		t.Fatalf("secret %v", raw)
	}
}
