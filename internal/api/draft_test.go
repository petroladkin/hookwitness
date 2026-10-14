package api

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/petroladkin/hookwitness/internal/store"
)

func TestSchemaDraft(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/shop/events")
	other := e.endpoint(t, "/other")
	_, tag := e.behaviour(t, ep.ID)
	e.putBehaviour(ep.ID, tag, `{"schemas":[{"name":"OrderPaid","text":"{\"type\":\"object\",\"properties\":{\"id\":{\"type\":\"integer\"}},\"required\":[\"id\"]}"}],
		"checks":[{"kind":"schema","select":{"from":"body","key":"/type","map":[{"value":"order.paid","schema":"OrderPaid"}]}}]}`)
	a := e.add(t, ep.ID, `{"type":"order.shipped","id":1,"at":"2026-10-12T17:28:07Z"}`, nil)
	b := e.add(t, ep.ID, `{"type":"order.shipped","id":2,"note":null}`, nil)
	paid := e.add(t, ep.ID, `{"type":"order.paid","id":1.5}`, nil)
	notJSON := e.add(t, ep.ID, `<xml/>`, nil)
	big := e.add(t, ep.ID, "", func(r *store.Request) { r.Body, r.TooLarge, r.Size = nil, true, 1<<30 })
	elsewhere := e.add(t, other.ID, `{}`, nil)
	sch, err := compiler(t).Compile(docURL + "#/components/schemas/SchemaDraft")
	if err != nil {
		t.Fatal(err)
	}
	draft := func(ids ...int64) map[string]any {
		t.Helper()
		var s []string
		for _, id := range ids {
			s = append(s, fmt.Sprint(id))
		}
		rec := e.do("POST", fmt.Sprintf("/endpoints/%d/schema-draft", ep.ID), `{"requests":[`+strings.Join(s, ",")+`]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("draft %v: %d %s", ids, rec.Code, rec.Body)
		}
		v, _ := jsonschema.UnmarshalJSON(bytes.NewReader(rec.Body.Bytes()))
		if err := sch.Validate(v); err != nil {
			t.Fatalf("the answer does not match openapi.json: %v", err)
		}
		return decodeAs[map[string]any](t, rec)
	}
	got := draft(a.ID, b.ID)
	text := got["text"].(string)
	if got["name"] != "OrderShipped" || got["exists"] != false || got["changes"] != nil ||
		!strings.Contains(text, `"required": ["type", "id"]`) || !strings.Contains(text, `"type": "null"`) {
		t.Fatalf("draft %v", got)
	}
	if d := got["discriminator"].(map[string]any); d["check"] != 1.0 || d["value"] != "order.shipped" || d["mapped"] != "" {
		t.Fatalf("discriminator %v", d)
	}
	got = draft(paid.ID)
	if got["name"] != "OrderPaid" || got["exists"] != true || fmt.Sprint(got["changes"]) != "[/id: type integer → number /type: new key, required]" {
		t.Fatalf("re-draft %v", got)
	}
	if got := draft(a.ID, paid.ID); got["name"] != "Events" || got["discriminator"].(map[string]any)["value"] != "" {
		t.Fatalf("mixed values %v", got)
	}
	if rec := e.do("GET", fmt.Sprintf("/endpoints/%d/behaviour", ep.ID), ""); strings.Contains(rec.Body.String(), "OrderShipped") {
		t.Fatal("a draft was stored")
	}
	for body, msg := range map[string]string{
		`{"requests":[]}`: "at least one",
		fmt.Sprintf(`{"requests":[%d]}`, notJSON.ID):        "not JSON",
		fmt.Sprintf(`{"requests":[%d]}`, big.ID):            "not kept",
		fmt.Sprintf(`{"requests":[%d]}`, elsewhere.ID):      "not one of this endpoint's",
		`{"requests":[999]}`:                                "not one of this endpoint's",
		`{"requests":[` + strings.Repeat("1,", 100) + `1]}`: "at most 100",
	} {
		got := wantError(t, e.do("POST", fmt.Sprintf("/endpoints/%d/schema-draft", ep.ID), body), http.StatusUnprocessableEntity, "invalid", "requests")
		if !strings.Contains(got.Error.Message, msg) {
			t.Errorf("%s: %q, want %q", body, got.Error.Message, msg)
		}
	}
	wantError(t, e.do("POST", fmt.Sprintf("/endpoints/%d/schema-draft", ep.ID), `{"ids":[1]}`), http.StatusBadRequest, "unknown_field", "ids")
}
