package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

func (e env) config(t *testing.T, id int64) emulate.Config {
	t.Helper()
	ep, err := e.st.Endpoint(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	c, err := emulate.Parse(ep.Config)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (e env) edit(endpoint int64, form url.Values) *httptest.ResponseRecorder {
	return e.do("POST", "/admin/e/"+id(endpoint)+"/behaviour", form)
}

func TestBehaviourEdits(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/b")
	ok := func(form url.Values) {
		t.Helper()
		if rec := e.edit(ep.ID, form); rec.Code != http.StatusSeeOther {
			t.Fatalf("%v: %d %s", form, rec.Code, errorOf(rec.Body.String()))
		}
	}
	ok(url.Values{"op": {"default"}, "status": {"202"}, "headers": {"Content-Type: application/json\nX-A: 1\nX-A: 2"},
		"body": {`{"n":{{.N}}}`}, "delay": {"100"}, "jitter": {"20"}})
	ok(url.Values{"op": {"rule-add"}})
	ok(url.Values{"op": {"rule-add"}})
	ok(url.Values{"op": {"rule-save"}, "i": {"0"}, "when": {"first"}, "n": {"2"}, "do": {"reply"}, "status": {"503"}, "body": {"busy"}})
	ok(url.Values{"op": {"rule-save"}, "i": {"1"}, "when": {"random"}, "percent": {"12.5"}, "do": {"drop"}, "status": {"500"}})
	ok(url.Values{"op": {"rule-down"}, "i": {"0"}})
	ok(url.Values{"op": {"check-add"}})
	ok(url.Values{"op": {"check-save"}, "i": {"0"}, "header": {"Authorization"}, "match": {"regex"}, "value": {"^Bearer "},
		"own_error": {"on"}, "error_status": {"401"}, "error_body": {"no"}})
	ok(url.Values{"op": {"vars"}, "vars": {"TOKEN=tok_test\nSECRET=whsec_test"}})

	c := e.config(t, ep.ID)
	if c.Default.Status != 202 || len(c.Default.Headers) != 3 || c.Default.DelayMs != 100 || c.Default.JitterMs != 20 {
		t.Fatalf("default %+v", c.Default)
	}
	if len(c.Rules) != 2 || c.Rules[0].Do != emulate.ActDrop || c.Rules[0].When.Percent != 12.5 ||
		c.Rules[1].When.Kind != emulate.WhenFirst || c.Rules[1].Reply.Status != 503 {
		t.Fatalf("rules after moving the first down: %+v", c.Rules)
	}
	if len(c.Checks) != 1 || c.Checks[0].Error == nil || c.Checks[0].Error.Status != 401 || c.Checks[0].Value != "^Bearer " {
		t.Fatalf("checks %+v", c.Checks)
	}
	if len(c.Vars) != 2 || c.Vars[1].Value != "whsec_test" {
		t.Fatalf("vars %+v", c.Vars)
	}

	page := e.do("GET", "/admin/e/"+id(ep.ID)+"/behaviour", nil).Body.String()
	for _, want := range []string{"12.5% → drop the connection", "first 2 → 503", "header Authorization matches /^Bearer /",
		"TOKEN=tok_test", "X-A: 2", "{{.JSON", "5 min"} {
		if !strings.Contains(page, strings.ReplaceAll(want, `"`, "&#34;")) && !strings.Contains(page, want) {
			t.Errorf("behaviour page lacks %q", want)
		}
	}

	ok(url.Values{"op": {"rule-del"}, "i": {"0"}})
	ok(url.Values{"op": {"check-del"}, "i": {"0"}})
	c = e.config(t, ep.ID)
	if len(c.Rules) != 1 || c.Rules[0].When.Kind != emulate.WhenFirst || len(c.Checks) != 0 {
		t.Fatalf("after deletes: %+v", c)
	}
}

func errorOf(page string) string {
	if i := strings.Index(page, "Not saved: "); i >= 0 {
		return page[i : i+min(200, len(page)-i)]
	}
	return ""
}

func TestBehaviourRefusesInvalid(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/v")
	e.edit(ep.ID, url.Values{"op": {"rule-add"}})
	before := e.config(t, ep.ID).String()
	for form, want := range map[string]url.Values{
		"status 700: use 200-599":        {"op": {"default"}, "status": {"700"}},
		"set by the server":              {"op": {"default"}, "status": {"200"}, "headers": {"Content-Length: 3"}},
		"write it as Name: value":        {"op": {"default"}, "status": {"200"}, "headers": {"oops"}},
		"template error":                 {"op": {"default"}, "status": {"200"}, "body": {"{{.JSON"}},
		"over the maximum wait of 5m0s":  {"op": {"default"}, "status": {"200"}, "delay": {"300001"}},
		"must be a whole number":         {"op": {"default"}, "status": {"two hundred"}},
		"the count N must be at least 1": {"op": {"rule-save"}, "i": {"0"}, "when": {"every"}, "n": {"0"}, "do": {"timeout"}},
		"that rule no longer exists":     {"op": {"rule-save"}, "i": {"5"}},
		"regular expression":             {"op": {"check-add"}},
		"defined twice":                  {"op": {"vars"}, "vars": {"A=1\nA=2"}},
		"unknown edit":                   {"op": {"explode"}},
	} {
		if form == "regular expression" {
			continue // needs an existing check: below
		}
		rec := e.edit(ep.ID, want)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Not saved: ") ||
			!strings.Contains(errorOf(rec.Body.String()), strings.ReplaceAll(form, `"`, "&#34;")) {
			t.Errorf("%s: %d %q", form, rec.Code, errorOf(rec.Body.String()))
		}
	}
	if e.config(t, ep.ID).String() != before {
		t.Fatal("a refused edit changed the stored behaviour")
	}
	e.edit(ep.ID, url.Values{"op": {"check-add"}})
	withCheck := e.config(t, ep.ID).String()
	if rec := e.edit(ep.ID, url.Values{"op": {"check-save"}, "i": {"0"}, "header": {"X"}, "match": {"regex"}, "value": {"("}}); !strings.Contains(errorOf(rec.Body.String()), "regular expression") {
		t.Errorf("bad regex: %q", errorOf(rec.Body.String()))
	}
	if e.config(t, ep.ID).String() != withCheck {
		t.Fatal("a refused check edit changed the stored behaviour")
	}
	// What was typed comes back with the error.
	rec := e.edit(ep.ID, url.Values{"op": {"default"}, "status": {"200"}, "body": {"{{.JSON"}})
	if !strings.Contains(rec.Body.String(), "{{.JSON</textarea>") {
		t.Error("the refused body is not shown again for correction")
	}
}

func TestCounterReset(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/c")
	for range 3 {
		e.st.AddRequest(context.Background(), &store.Request{EndpointID: ep.ID, Method: "POST", Path: "/c", Status: 200}, 1000,
			func(int64) (int, string) { return 200, "" })
	}
	if !strings.Contains(e.do("GET", "/admin/e/"+id(ep.ID)+"/behaviour", nil).Body.String(), `<b class="big">3</b>`) {
		t.Fatal("counter not shown")
	}
	if rec := e.edit(ep.ID, url.Values{"op": {"counter-reset"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("reset: %d", rec.Code)
	}
	if got, _ := e.st.Endpoint(context.Background(), ep.ID); got.Counter != 0 {
		t.Fatalf("counter %d after reset", got.Counter)
	}
	if rec := e.do("GET", "/admin/e/unmatched/behaviour", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("Unmatched has no behaviour: %d", rec.Code)
	}
}

func TestExhibitShowsTheAnswer(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/a")
	out := emulate.Outcome{Source: emulate.FromRule, Rule: 2, Label: "every 3rd", Action: emulate.ActReply, Status: 500,
		Headers: []emulate.Header{{Name: "Retry-After", Value: "2"}}, Body: "<down>", DelayMs: 1500, Counter: 3}
	r := e.add(t, ep.ID, "{}", func(r *store.Request) { r.Status, r.Outcome = 500, out.String() })
	timeout := emulate.Outcome{Source: emulate.FromDefault, Action: emulate.ActTimeout, WaitedMs: 30000, Counter: 4}
	tr := e.add(t, ep.ID, "{}", func(r *store.Request) { r.Status, r.Outcome = 0, timeout.String() })

	page := e.do("GET", "/admin/e/"+id(ep.ID)+"/r/"+id(r.ID), nil).Body.String()
	for _, want := range []string{"500 sent <small>rule 2 · every 3rd</small>", "500 sent after 1.5 s · rule 2 · every 3rd · request 3 since the counter reset.",
		"<th>Retry-After</th><td>2</td>", "&lt;down&gt;", `data-mark="bad"`, "500 · rule 2", `data-mark="timeout"`} {
		if !strings.Contains(page, want) {
			t.Errorf("exhibit lacks %q", want)
		}
	}
	tp := e.do("GET", "/admin/e/"+id(ep.ID)+"/r/"+id(tr.ID), nil).Body.String()
	if !strings.Contains(tp, "Held without an answer for 30.0 s, then closed") || !strings.Contains(tp, ">timeout <small>default reply</small>") {
		t.Error("timeout exhibit lacks its stamp or answer")
	}
}
