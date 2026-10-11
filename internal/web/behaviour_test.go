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

// edit posts a behaviour form as the page would, with the current version
// unless the form names one.
func (e env) edit(endpoint int64, form url.Values) *httptest.ResponseRecorder {
	if _, ok := form["v"]; !ok {
		ep, _ := e.st.Endpoint(context.Background(), endpoint)
		form.Set("v", configVersion(ep.Config))
	}
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
	ok(url.Values{"op": {"rule-save"}, "i": {"new"}, "when": {"every"}, "n": {"2"}, "do": {"reply"}, "status": {"500"}})
	ok(url.Values{"op": {"rule-save"}, "i": {"new"}, "when": {"every"}, "n": {"2"}, "do": {"reply"}, "status": {"500"}})
	ok(url.Values{"op": {"rule-save"}, "i": {"0"}, "when": {"first"}, "n": {"2"}, "do": {"reply"}, "status": {"503"}, "body": {"busy"}})
	ok(url.Values{"op": {"rule-save"}, "i": {"1"}, "when": {"random"}, "percent": {"12.5"}, "do": {"drop"}, "status": {"500"}})
	ok(url.Values{"op": {"rule-down"}, "i": {"0"}})
	ok(url.Values{"op": {"check-save"}, "i": {"new"}, "header": {"Authorization"}, "match": {"regex"}, "value": {"^Bearer "},
		"own_error": {"on"}, "error_status": {"401"}, "error_body": {"no"}})
	ok(url.Values{"op": {"var-save"}, "i": {"new"}, "name": {"TOKEN"}, "value": {"tok_test"}})
	ok(url.Values{"op": {"var-save"}, "i": {"new"}, "name": {"SECRET"}, "value": {"wrong"}})
	ok(url.Values{"op": {"var-save"}, "i": {"1"}, "name": {"SECRET"}, "value": {"whsec_test"}})

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

	for tab, wants := range map[string][]string{
		"rules":   {"12.5%</b> → drop the connection", "first 2</b> → 503"},
		"checks":  {"<b>Authorization</b> matches /^Bearer / · else 401 (own reply)"},
		"default": {"X-A: 2", "100 ms + up to 20 ms at random"},
		"vars":    {"<b>TOKEN</b> = tok_test", "<b>SECRET</b> = whsec_test"},
	} {
		page := e.do("GET", "/admin/e/"+id(ep.ID)+"/behaviour?tab="+tab, nil).Body.String()
		for _, want := range wants {
			if !strings.Contains(page, strings.ReplaceAll(want, `"`, "&#34;")) && !strings.Contains(page, want) {
				t.Errorf("behaviour tab %s lacks %q", tab, want)
			}
		}
	}

	ok(url.Values{"op": {"rule-del"}, "i": {"0"}})
	ok(url.Values{"op": {"check-del"}, "i": {"0"}})
	ok(url.Values{"op": {"var-del"}, "i": {"0"}})
	c = e.config(t, ep.ID)
	if len(c.Rules) != 1 || c.Rules[0].When.Kind != emulate.WhenFirst || len(c.Checks) != 0 || len(c.Vars) != 1 || c.Vars[0].Name != "SECRET" {
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
	e.edit(ep.ID, url.Values{"op": {"rule-save"}, "i": {"new"}, "when": {"every"}, "n": {"2"}, "do": {"timeout"}})
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
		"regular expression":             {"op": {"check-save"}},
		"a name is letters":              {"op": {"var-save"}, "i": {"new"}, "name": {"1A"}},
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
	e.edit(ep.ID, url.Values{"op": {"check-save"}, "i": {"new"}, "header": {"X"}, "match": {"present"}})
	withCheck := e.config(t, ep.ID).String()
	if rec := e.edit(ep.ID, url.Values{"op": {"check-save"}, "i": {"0"}, "header": {"X"}, "match": {"regex"}, "value": {"("}}); !strings.Contains(errorOf(rec.Body.String()), "regular expression") {
		t.Errorf("bad regex: %q", errorOf(rec.Body.String()))
	}
	if e.config(t, ep.ID).String() != withCheck {
		t.Fatal("a refused check edit changed the stored behaviour")
	}
	// What was typed comes back with the error, in the editor it was typed in.
	rec := e.edit(ep.ID, url.Values{"op": {"default"}, "status": {"200"}, "body": {"{{.JSON"}})
	if body := rec.Body.String(); !strings.Contains(body, "{{.JSON</textarea>") || !strings.Contains(body, `<h2 id="dialog-title">Default reply</h2>`) {
		t.Error("the refused body is not shown again for correction")
	}
	e.edit(ep.ID, url.Values{"op": {"var-save"}, "i": {"new"}, "name": {"A"}, "value": {"1"}})
	rec = e.edit(ep.ID, url.Values{"op": {"var-save"}, "i": {"new"}, "name": {"A"}, "value": {"2"}})
	if body := rec.Body.String(); !strings.Contains(errorOf(body), "defined twice") || !strings.Contains(body, `<h2 id="dialog-title">New variable</h2>`) ||
		!strings.Contains(body, `name="i" value="new"`) || strings.Count(body, "<b>A</b>") != 1 {
		t.Error("a refused new variable reopens its editor as new, and the list shows only what is stored")
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

// TestStaleEditRefused: a page loaded before another change must not apply
// its edit to whatever rule now sits at its index.
func TestStaleEditRefused(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/s")
	e.edit(ep.ID, url.Values{"op": {"rule-save"}, "i": {"new"}, "when": {"every"}, "n": {"2"}, "do": {"timeout"}})
	e.edit(ep.ID, url.Values{"op": {"rule-save"}, "i": {"new"}, "when": {"every"}, "n": {"5"}, "do": {"drop"}})
	page := e.do("GET", "/admin/e/"+id(ep.ID)+"/behaviour", nil).Body.String()
	stale := configVersion(func() string { x, _ := e.st.Endpoint(context.Background(), ep.ID); return x.Config }())
	if !strings.Contains(page, `name="v" value="`+stale+`"`) {
		t.Fatal("the page does not carry the version")
	}
	// Another tab deletes rule 1; this page still thinks rule 2 is at index 1.
	e.edit(ep.ID, url.Values{"op": {"rule-del"}, "i": {"0"}})
	before := e.config(t, ep.ID).String()
	rec := e.edit(ep.ID, url.Values{"op": {"rule-del"}, "i": {"1"}, "v": {stale}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "changed elsewhere") {
		t.Fatalf("stale edit: %d %s", rec.Code, errorOf(rec.Body.String()))
	}
	if e.config(t, ep.ID).String() != before {
		t.Fatal("a stale edit changed the behaviour")
	}
	if rec := e.edit(ep.ID, url.Values{"op": {"rule-del"}, "i": {"x"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("garbled index: %d", rec.Code)
	}
	if e.config(t, ep.ID).String() != before || len(e.config(t, ep.ID).Rules) != 1 {
		t.Fatal("a garbled index must not fall back to rule 1")
	}
	// Reset needs no version: it changes no configuration.
	if rec := e.do("POST", "/admin/e/"+id(ep.ID)+"/behaviour", url.Values{"op": {"counter-reset"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("reset without a version: %d", rec.Code)
	}
}

// TestBehaviourTabs: one list per tab, rules by default, the main action in
// the toolbar; an editor opens by URL; a saved edit lands on its tab and says
// so; a refused one keeps its editor open on its tab.
func TestBehaviourTabs(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/t")
	base := "/admin/e/" + id(ep.ID) + "/behaviour"

	page := e.do("GET", base, nil).Body.String()
	if !strings.Contains(page, `href="`+base+`?tab=rules" aria-current="page"`) || !strings.Contains(page, "Reset counter") ||
		!strings.Contains(page, `href="`+base+`?tab=rules&amp;edit=rule-new">New rule</a>`) || strings.Contains(page, "<dialog") {
		t.Fatal("the page opens on the rules list, with New rule in the toolbar and no dialog")
	}
	if page := e.do("GET", base+"?tab=bogus", nil).Body.String(); !strings.Contains(page, `?tab=rules" aria-current="page"`) {
		t.Fatal("an unknown tab falls back to rules")
	}
	for tab, action := range map[string]string{"checks": "check-new\">New check", "default": "default\">Edit default reply", "vars": "var-new\">New variable"} {
		if page := e.do("GET", base+"?tab="+tab, nil).Body.String(); !strings.Contains(page, `class="toolbar"`) || !strings.Contains(page, action) {
			t.Errorf("tab %s lacks its toolbar action", tab)
		}
	}

	for _, c := range []struct {
		form url.Values
		to   string
	}{
		{url.Values{"op": {"rule-save"}, "i": {"new"}, "when": {"every"}, "n": {"2"}, "do": {"reply"}, "status": {"500"}}, "?tab=rules&saved=rule-0"},
		{url.Values{"op": {"rule-save"}, "i": {"0"}, "when": {"every"}, "n": {"3"}, "do": {"timeout"}}, "?tab=rules&saved=rule-0"},
		{url.Values{"op": {"check-save"}, "i": {"new"}, "header": {"X-Env"}, "match": {"present"}}, "?tab=checks&saved=check-0"},
		{url.Values{"op": {"default"}, "status": {"204"}}, "?tab=default&saved=default"},
		{url.Values{"op": {"var-save"}, "i": {"new"}, "name": {"A"}, "value": {"1"}}, "?tab=vars&saved=var-0"},
		{url.Values{"op": {"counter-reset"}}, "?tab=rules&saved=counter"},
		{url.Values{"op": {"rule-del"}, "i": {"0"}}, "?tab=rules&saved=rules"},
	} {
		rec := e.edit(ep.ID, c.form)
		if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || loc != base+c.to {
			t.Errorf("%v: %d to %q, want %q", c.form, rec.Code, loc, base+c.to)
		}
	}

	e.edit(ep.ID, url.Values{"op": {"rule-save"}, "i": {"new"}, "when": {"first"}, "n": {"1"}, "do": {"drop"}})
	page = e.do("GET", base+"?tab=rules&saved=rule-0", nil).Body.String()
	if !strings.Contains(page, "Rule 1 saved. It applies to the next request.") || !strings.Contains(page, `<li id="rule-0" class="saved">`) {
		t.Error("a saved rule is not confirmed")
	}

	for edit, title := range map[string]string{"rule-new": "New rule", "rule-0": "Rule 1", "check-0": "Check 1", "check-new": "New check",
		"default": "Default reply", "var-0": "Variable A", "var-new": "New variable"} {
		page := e.do("GET", base+"?edit="+edit, nil).Body.String()
		if !strings.Contains(page, `<dialog class="modal" data-modal`) || !strings.Contains(page, `<h2 id="dialog-title">`+title+`</h2>`) ||
			!strings.Contains(page, `aria-current="page"`) {
			t.Errorf("edit=%s: no %q dialog", edit, title)
		}
	}
	if page := e.do("GET", base+"?edit=rule-7", nil).Body.String(); strings.Contains(page, "<dialog") {
		t.Error("an editor for a rule that does not exist")
	}

	rec := e.edit(ep.ID, url.Values{"op": {"rule-save"}, "i": {"0"}, "when": {"every"}, "n": {"0"}, "do": {"drop"}})
	if body := rec.Body.String(); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, `?tab=rules" aria-current="page"`) ||
		!strings.Contains(body, `<h2 id="dialog-title">Rule 1</h2>`) || !strings.Contains(body, "Not saved: ") {
		t.Errorf("a refused rule keeps its editor open: %d", rec.Code)
	}
	rec = e.edit(ep.ID, url.Values{"op": {"check-del"}, "i": {"0"}, "v": {"stale"}})
	if body := rec.Body.String(); rec.Code != http.StatusConflict || !strings.Contains(body, `?tab=checks" aria-current="page"`) || strings.Contains(body, "<dialog") {
		t.Errorf("a stale check edit shows on the checks tab: %d", rec.Code)
	}
}

func TestNewEndpointDialog(t *testing.T) {
	e := newEnv(t)
	page := e.do("GET", "/admin/", nil).Body.String()
	if !strings.Contains(page, `href="/admin/?edit=new">New endpoint</a>`) || strings.Contains(page, "<dialog") {
		t.Fatal("the list has New endpoint in its toolbar and no form")
	}
	if page := e.do("GET", "/admin/?edit=new", nil).Body.String(); !strings.Contains(page, `<h2 id="dialog-title">New endpoint</h2>`) {
		t.Fatal("no new-endpoint dialog")
	}
	rec := e.do("POST", "/admin/endpoints", url.Values{"path": {"no-slash"}})
	if body := rec.Body.String(); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "<dialog") ||
		!strings.Contains(body, `value="no-slash"`) || !strings.Contains(body, "must start with") {
		t.Fatalf("a refused path keeps the dialog open: %d", rec.Code)
	}
}

// TestSecretsWriteOnly: a saved secret's value appears on no page, and the
// stored configuration keeps it only sealed.
func TestSecretsWriteOnly(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/sec")
	if rec := e.edit(ep.ID, url.Values{"op": {"secret-save"}, "i": {"new"}, "name": {"WEBHOOK_SECRET"}, "secret_kind": {"key"}, "value": {"whsec_test_plain_1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rec.Code, errorOf(rec.Body.String()))
	}
	if rec := e.edit(ep.ID, url.Values{"op": {"secret-save"}, "i": {"new"}, "name": {"API_TOKEN"}, "secret_kind": {"token"}, "value": {"tok_test_plain_2"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save token: %d %s", rec.Code, errorOf(rec.Body.String()))
	}
	ep, _ = e.st.Endpoint(context.Background(), ep.ID)
	for _, plain := range []string{"whsec_test_plain_1", "tok_test_plain_2"} {
		if strings.Contains(ep.Config, plain) {
			t.Fatalf("stored config holds %q", plain)
		}
	}
	c := e.config(t, ep.ID)
	if c.Secrets[0].Sealed == "" || c.Secrets[0].Hash != "" || c.Secrets[1].Hash == "" || c.Secrets[1].Sealed != "" || len(c.Secrets[0].Fingerprint) != 8 {
		t.Fatalf("secrets %+v", c.Secrets)
	}
	base := "/admin/e/" + id(ep.ID) + "/behaviour"
	for _, q := range []string{"", "?tab=secrets", "?tab=checks", "?tab=vars", "?edit=secret-0", "?edit=secret-1", "?edit=secret-new", "?edit=check-new", "?tab=secrets&saved=secret-0"} {
		page := e.do("GET", base+q, nil).Body.String()
		for _, plain := range []string{"whsec_test_plain_1", "tok_test_plain_2"} {
			if strings.Contains(page, plain) {
				t.Errorf("%s shows %q", q, plain)
			}
		}
	}
	page := e.do("GET", base+"?tab=secrets", nil).Body.String()
	if !strings.Contains(page, "<b>WEBHOOK_SECRET</b> · signing key · fingerprint <code>"+c.Secrets[0].Fingerprint+"</code> · not used") {
		t.Errorf("secrets list lacks the row")
	}
	// A refused save does not echo the value back either.
	rec := e.edit(ep.ID, url.Values{"op": {"secret-save"}, "i": {"new"}, "name": {"1BAD"}, "secret_kind": {"key"}, "value": {"whsec_test_plain_3"}})
	if rec.Code != http.StatusUnprocessableEntity || strings.Contains(rec.Body.String(), "whsec_test_plain_3") || !strings.Contains(rec.Body.String(), `<h2 id="dialog-title">New secret</h2>`) {
		t.Errorf("refused secret: %d", rec.Code)
	}
	// Replacing keeps the name and kind, changes the value.
	old := c.Secrets[0].Sealed
	if rec := e.edit(ep.ID, url.Values{"op": {"secret-save"}, "i": {"0"}, "name": {"OTHER"}, "secret_kind": {"token"}, "value": {"whsec_test_plain_4"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("replace: %d %s", rec.Code, errorOf(rec.Body.String()))
	}
	c = e.config(t, ep.ID)
	if c.Secrets[0].Name != "WEBHOOK_SECRET" || c.Secrets[0].Kind != "key" || c.Secrets[0].Sealed == old {
		t.Fatalf("after replace %+v", c.Secrets[0])
	}
	if rec := e.edit(ep.ID, url.Values{"op": {"secret-save"}, "i": {"new"}, "name": {"EMPTY"}, "secret_kind": {"key"}, "value": {""}}); !strings.Contains(errorOf(rec.Body.String()), "enter the value") {
		t.Errorf("empty value: %q", errorOf(rec.Body.String()))
	}
}

func TestAuthCheckEditor(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/auth")
	save := url.Values{"op": {"check-save"}, "i": {"new"}, "kind": {"stripe"}, "secret": {"WEBHOOK_SECRET"}, "tolerance_s": {"600"}, "enforce": {"on"}}
	rec := e.edit(ep.ID, save)
	if body := rec.Body.String(); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(errorOf(body), "secret WEBHOOK_SECRET does not exist") ||
		!strings.Contains(body, `<h2 id="dialog-title">New check</h2>`) {
		t.Fatalf("unknown secret: %d %q", rec.Code, errorOf(body))
	}
	e.edit(ep.ID, url.Values{"op": {"secret-save"}, "i": {"new"}, "name": {"WEBHOOK_SECRET"}, "secret_kind": {"key"}, "value": {"whsec_test_1"}})
	save.Del("v") // edit filled in the version of that time
	if rec := e.edit(ep.ID, save); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rec.Code, errorOf(rec.Body.String()))
	}
	c := e.config(t, ep.ID)
	if ch := c.Checks[0]; ch.Kind != "stripe" || ch.Secret != "WEBHOOK_SECRET" || ch.ToleranceS != 600 || !ch.Enforce || ch.Header != "" {
		t.Fatalf("stored check %+v", ch)
	}
	base := "/admin/e/" + id(ep.ID) + "/behaviour"
	page := e.do("GET", base+"?tab=checks", nil).Body.String()
	if !strings.Contains(page, "<b>Stripe signature</b> · secret WEBHOOK_SECRET · tolerance 600 s · enforced · else 401") {
		t.Errorf("checks list lacks the row")
	}
	rec = e.edit(ep.ID, url.Values{"op": {"secret-del"}, "i": {"0"}})
	if !strings.Contains(errorOf(rec.Body.String()), "WEBHOOK_SECRET is used by check 1 (Stripe signature)") {
		t.Errorf("deleting a used secret: %q", errorOf(rec.Body.String()))
	}
	dialog := e.do("GET", base+"?edit=check-new", nil).Body.String()
	if strings.Count(dialog, `<option value="`) < 7+1+3 || !strings.Contains(dialog, `<option value="WEBHOOK_SECRET">WEBHOOK_SECRET (signing key)</option>`) {
		t.Errorf("check dialog lacks kinds or secrets")
	}
	// A bearer check refuses a signing key; a header check still saves as before.
	e.edit(ep.ID, url.Values{"op": {"secret-save"}, "i": {"new"}, "name": {"TOKEN"}, "secret_kind": {"token"}, "value": {"tok_test"}})
	if rec := e.edit(ep.ID, url.Values{"op": {"check-save"}, "i": {"new"}, "kind": {"bearer"}, "secret": {"WEBHOOK_SECRET"}}); !strings.Contains(errorOf(rec.Body.String()), "is a signing key, not a token or password") {
		t.Errorf("bearer with a key: %q", errorOf(rec.Body.String()))
	}
	if rec := e.edit(ep.ID, url.Values{"op": {"check-save"}, "i": {"new"}, "kind": {"header"}, "header": {"X-Env"}, "match": {"present"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("header check: %d %s", rec.Code, errorOf(rec.Body.String()))
	}
	if ch := e.config(t, ep.ID).Checks[1]; ch.Kind != "" || ch.Header != "X-Env" || ch.Secret != "" {
		t.Fatalf("header check stored as %+v", ch)
	}
}
