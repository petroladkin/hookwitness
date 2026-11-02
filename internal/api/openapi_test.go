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
	"time"

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
		"rules":[{"when":{"kind":"every","n":3},"do":"reply","reply":{"status":503,"body_base64":"//4="}},
			{"when":{"kind":"window","from_s":1,"to_s":2.5},"do":"drop"},{"when":{"kind":"flap","down":1,"up":2,"unit":"requests"},"do":"timeout"},
			{"when":{"kind":"rate","n":5,"per_s":10,"block_s":30},"do":"reply","reply":{"status":429,"headers":[{"name":"Retry-After","value":"{{.RetryAfter}}"}]}},
			{"when":{"kind":"attempts","n":1},"do":"reply","reply":{"status":503}}],
		"checks":[{"kind":"basic","secret":"PW","user":"u"},{"kind":"schema","schema":"S"}],
		"vars":[{"name":"V","value":"x"}],"schemas":[{"name":"S","text":"{\"type\":\"object\"}"}]}`, "If-Match", tag)
	r := e.add(t, ep.ID, `{"a":1}`, func(r *store.Request) {
		r.Verdict = "fail"
		r.Verdicts = `[{"check":1,"label":"Basic credentials","result":"fail","reason":"no Authorization header","at":"2026-10-13T10:00:00Z"},
			{"check":2,"label":"JSON Schema S","result":"pass","reason":"matches S","at":"2026-10-13T10:00:00Z",
			"note":"same signature as #1: a replay, or a redelivery of the same signed request"}]`
		r.Outcome = `{"source":"default","action":"reply","status":202}`
	})
	e.add(t, store.Unmatched, "", func(r *store.Request) { r.Path, r.Status = "/typo", 404 })
	// A request the rules decided starts the clock: started_at is a time.
	e.st.AddRequest(t.Context(), &store.Request{EndpointID: ep.ID, Method: "POST", Path: "/hooks", ReceivedAt: time.Now()}, store.Keep{Normal: 1000},
		func(in store.DecideIn) (int, string, string) {
			return 429, `{"source":"rule","rule":4,"label":"over 5 in 10 s: 30 s","action":"reply","status":429,"elapsed_ms":0,"block_until":"2026-11-03T10:00:30.000Z"}`,
				fmt.Sprintf(`{"start":%d}`, in.At.UnixMilli())
		})
	// A forwarding endpoint with a request the receiver answered.
	fwd := e.endpoint(t, "/forwarding")
	_, ftag := e.behaviour(t, fwd.ID)
	e.putBehaviour(fwd.ID, ftag, `{"forward":{"url":"https://receiver.test/in","keep_host":true,"on_failure":"default"},
		"rules":[{"when":{"kind":"every","n":2},"do":"reply","reply":{"status":503},"forward":true}]}`)
	fr := e.add(t, fwd.ID, "{}", func(r *store.Request) {
		r.Outcome = `{"source":"forward","action":"forward","forward":{"target":"https://receiver.test/in","pending":true,"ms":0}}`
	})
	e.st.FinishForward(t.Context(), &store.Forward{RequestID: fr.ID, CreatedAt: time.Now(), URL: "https://receiver.test/in",
		Options: `{"host":"received","insecure_tls":false,"timeout_ms":30000}`, Line: "POST /in HTTP/1.1", Sent: []store.Header{{Name: "Host", Value: "h"}},
		Proto: "HTTP/1.1", Status: 502, Reason: "Bad Gateway", Body: []byte{0xff}, Size: 1, ErrorKind: "", Duration: time.Millisecond},
		502, `{"source":"forward","action":"forward","status":502,"forward":{"target":"https://receiver.test/in","status":502,"ms":1}}`)
	e.add(t, fwd.ID, "{}", func(r *store.Request) {
		r.Outcome = `{"source":"forward","action":"forward","status":504,"forward":{"target":"https://receiver.test/in","error_kind":"timeout","fallback":true,"sender_left_ms":10,"ms":30000}}`
	})
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
		{"GET", "/setup", "", "Setup"},
		{"POST", "/setup?on_conflict=replace", `{"hookwitness":1,"endpoints":[{"path":"/hooks","default":{"status":200},"secrets":[{"name":"PW","kind":"token"}]},{"path":"/new","default":{"status":200},"secrets":[{"name":"X","kind":"key"}]}]}`, "SetupResult"},
		{"GET", "/endpoints/2/behaviour", "", "Behaviour"},
		{"POST", "/setup", `{"hookwitness":2,"endpoints":[]}`, "Error"},
		{"GET", "/endpoints/999", "", "Error"},
		{"PATCH", fmt.Sprintf("/endpoints/%d", ep.ID), `{"path":"bad"}`, "Error"},
		{"GET", fmt.Sprintf("/endpoints/%d", fwd.ID), "", "Endpoint"},
		{"GET", fmt.Sprintf("/endpoints/%d/behaviour", fwd.ID), "", "Behaviour"},
		{"GET", fmt.Sprintf("/endpoints/%d/requests?forward=failed&include=body", fwd.ID), "", "RequestList"},
		{"GET", fmt.Sprintf("/requests/%d", fr.ID), "", "Request"},
		{"GET", fmt.Sprintf("/requests/%d/forward", fr.ID), "", "ForwardExchange"},
		{"GET", fmt.Sprintf("/requests/%d/forward", r.ID), "", "Error"},
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
