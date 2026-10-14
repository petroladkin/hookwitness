package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/petroladkin/hookwitness/internal/store"
)

const docURL = "https://hookwitness.invalid/openapi.json"

// compiler holds the official OpenAPI 3.1 schema and our document, so
// either can be compiled by URL.
func compiler(t *testing.T) *jsonschema.Compiler {
	t.Helper()
	c := jsonschema.NewCompiler()
	add := func(url, file string, b []byte) {
		if b == nil {
			var err error
			if b, err = os.ReadFile(file); err != nil {
				t.Fatal(err)
			}
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if err := c.AddResource(url, doc); err != nil {
			t.Fatal(err)
		}
	}
	add("https://spec.openapis.org/oas/3.1/schema/2022-10-07", "../../testdata/openapi/schema-3.1-2022-10-07.json", nil)
	add(docURL, "openapi.json", openAPI)
	return c
}

// TestOpenAPIIsValid: the document is a valid OpenAPI 3.1 document.
func TestOpenAPIIsValid(t *testing.T) {
	c := compiler(t)
	sch, err := c.Compile("https://spec.openapis.org/oas/3.1/schema/2022-10-07")
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := jsonschema.UnmarshalJSON(bytes.NewReader(openAPI))
	if err := sch.Validate(doc); err != nil {
		t.Fatalf("openapi.json is not valid OpenAPI 3.1: %v", err)
	}
}

// TestOpenAPICoversEveryRoute: every route is documented with its method,
// and nothing is documented that is not a route.
func TestOpenAPICoversEveryRoute(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]json.RawMessage
	}
	if err := json.Unmarshal(openAPI, &doc); err != nil {
		t.Fatal(err)
	}
	var routes, documented []string
	for _, rt := range newEnv(t).h.routes() {
		routes = append(routes, rt.method+" "+rt.path)
	}
	routes = append(routes, "GET /openapi.json")
	for p, ops := range doc.Paths {
		for m := range ops {
			if m != "parameters" {
				documented = append(documented, strings.ToUpper(m)+" "+p)
			}
		}
	}
	slices.Sort(routes)
	slices.Sort(documented)
	if !slices.Equal(routes, documented) {
		t.Fatalf("routes and openapi.json differ:\nroutes     %v\ndocumented %v", routes, documented)
	}
}

// TestAnswersMatchTheirSchemas: real answers of the API validate against
// the schemas the document gives them, so the document cannot drift.
func TestAnswersMatchTheirSchemas(t *testing.T) {
	c := compiler(t)
	schema := func(name string) *jsonschema.Schema {
		sch, err := c.Compile(docURL + "#/components/schemas/" + name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return sch
	}
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	e.do("PUT", fmt.Sprintf("/endpoints/%d/secrets/PW", ep.ID), `{"kind":"token","value":"test-pw"}`)
	_, tag := e.behaviour(t, ep.ID)
	e.do("PUT", fmt.Sprintf("/endpoints/%d/behaviour", ep.ID), `{"default":{"status":202,"headers":[{"name":"X-A","value":"1"}]},
		"rules":[{"when":{"kind":"every","n":3},"do":"reply","reply":{"status":503}}],
		"checks":[{"kind":"basic","secret":"PW","user":"u"},{"kind":"schema","schema":"S"}],
		"vars":[{"name":"V","value":"x"}],"schemas":[{"name":"S","text":"{\"type\":\"object\"}"}]}`, "If-Match", tag)
	r := e.add(t, ep.ID, `{"a":1}`, func(r *store.Request) {
		r.Verdict = "fail"
		r.Verdicts = `[{"check":1,"label":"Basic credentials","result":"fail","reason":"no Authorization header","at":"2026-10-13T10:00:00Z"},
			{"check":2,"label":"JSON Schema S","result":"pass","reason":"matches S","at":"2026-10-13T10:00:00Z"}]`
		r.Outcome = `{"source":"default","action":"reply","status":202}`
	})
	e.add(t, store.Unmatched, "", func(r *store.Request) { r.Path, r.Status = "/typo", 404 })
	for _, tc := range []struct {
		method, path, body, schema string
	}{
		{"GET", fmt.Sprintf("/endpoints/%d", ep.ID), "", "Endpoint"},
		{"GET", fmt.Sprintf("/endpoints/%d/behaviour", ep.ID), "", "Behaviour"},
		{"GET", fmt.Sprintf("/endpoints/%d/requests", ep.ID), "", "RequestList"},
		{"GET", "/unmatched/requests", "", "RequestList"},
		{"GET", fmt.Sprintf("/endpoints/%d/requests?include=body&q=a&json=/a=1", ep.ID), "", "RequestList"},
		{"GET", fmt.Sprintf("/requests/%d", r.ID), "", "Request"},
		{"POST", fmt.Sprintf("/endpoints/%d/recheck", ep.ID), "{}", "Rechecked"},
		{"PUT", fmt.Sprintf("/endpoints/%d/secrets/K", ep.ID), `{"kind":"key","value":"whsec_dGVzdA=="}`, "Secret"},
		{"GET", "/endpoints/999", "", "Error"},
		{"PATCH", fmt.Sprintf("/endpoints/%d", ep.ID), `{"path":"bad"}`, "Error"},
	} {
		rec := e.do(tc.method, tc.path, tc.body)
		v, err := jsonschema.UnmarshalJSON(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		if err := schema(tc.schema).Validate(v); err != nil {
			t.Errorf("%s %s (%d) does not match %s: %v", tc.method, tc.path, rec.Code, tc.schema, err)
		}
	}
	if rec := e.do("GET", "/openapi.json", ""); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), openAPI) {
		t.Fatal("openapi.json is not served")
	}
}
