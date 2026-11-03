package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

// TestReceiverTab: the Receiver tab sets, shows and removes the receiver;
// an impossible one is refused with the reason and what was typed; the
// rule editor offers its switch only while the endpoint forwards.
func TestReceiverTab(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/live")
	base := "/admin/e/" + id(ep.ID) + "/behaviour"
	page := e.do("GET", base+"?tab=receiver", nil).Body.String()
	if !strings.Contains(page, "No receiver: this endpoint answers every request itself.") || !strings.Contains(page, ">Set receiver</a>") ||
		!strings.Contains(page, "Receiver") || !strings.Contains(page, "<small>off</small>") {
		t.Fatal("the empty receiver tab")
	}
	if page := e.do("GET", base+"?tab=rules&edit=rule-new", nil).Body.String(); strings.Contains(page, `name="forward"`) {
		t.Fatal("the rule's switch without a receiver")
	}
	form := url.Values{"op": {"receiver"}, "url": {"https://receiver.test/in"}, "host": {"received"}, "timeout": {"2.5"},
		"on_failure": {"default"}, "insecure_tls": {"on"}}
	if rec := e.edit(ep.ID, form); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rec.Code, errorOf(rec.Body.String()))
	}
	f := e.config(t, ep.ID).Forward
	if f == nil || *f != (emulate.Forward{URL: "https://receiver.test/in", KeepHost: true, InsecureTLS: true, TimeoutMs: 2500, OnFailure: emulate.OnFailureDefault}) {
		t.Fatalf("stored %+v", f)
	}
	page = e.do("GET", base+"?tab=receiver", nil).Body.String()
	for _, want := range []string{"<code>https://receiver.test/in</code>", "Host as received · any certificate · timeout 2.5 s · the default reply when it fails",
		"Anyone who reaches this endpoint", "Remove receiver", ">Edit receiver</a>", "<small>on</small>"} {
		if !strings.Contains(page, want) {
			t.Errorf("the receiver tab lacks %q", want)
		}
	}
	if page := e.do("GET", base+"?tab=default", nil).Body.String(); !strings.Contains(page, "Used when the receiver fails") {
		t.Error("the default tab does not say when it answers")
	}
	// The dialog shows what is stored.
	if page := e.do("GET", base+"?tab=receiver&edit=receiver", nil).Body.String(); !strings.Contains(page, `value="https://receiver.test/in"`) ||
		!strings.Contains(page, `value="2.5"`) || !strings.Contains(page, `<option value="received" selected>`) {
		t.Fatal("the receiver dialog")
	}
	// A rule with the switch.
	if rec := e.edit(ep.ID, url.Values{"op": {"rule-save"}, "i": {"new"}, "when": {"every"}, "n": {"3"}, "do": {"reply"}, "status": {"503"}, "forward": {"on"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("rule: %d %s", rec.Code, errorOf(rec.Body.String()))
	}
	e.edit(ep.ID, url.Values{"op": {"rule-save"}, "i": {"new"}, "when": {"every"}, "n": {"2"}, "do": {"drop"}})
	page = e.do("GET", base+"?tab=rules", nil).Body.String()
	if !strings.Contains(page, "· the receiver gets it too") || !strings.Contains(page, "· not forwarded") {
		t.Fatal("the rules list does not say which reach the receiver")
	}
	if page := e.do("GET", base+"?tab=rules&edit=rule-0", nil).Body.String(); !strings.Contains(page, `name="forward" checked`) {
		t.Fatal("the rule's switch")
	}
	// Refusals keep what was typed.
	before := e.config(t, ep.ID).String()
	for typed, want := range map[string]string{
		"https://receiver.test/in?token=1": "must not have a query",
		"http://169.254.169.254/latest":    "link-local",
		"ftp://receiver.test/":             "http or https",
	} {
		rec := e.edit(ep.ID, url.Values{"op": {"receiver"}, "url": {typed}})
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(errorOf(rec.Body.String()), want) ||
			!strings.Contains(rec.Body.String(), `value="`+strings.ReplaceAll(typed, "&", "&amp;")+`"`) {
			t.Errorf("%s: %d %q", typed, rec.Code, errorOf(rec.Body.String()))
		}
	}
	if e.config(t, ep.ID).String() != before {
		t.Fatal("a refused receiver changed the behaviour")
	}
	if rec := e.edit(ep.ID, url.Values{"op": {"receiver-del"}}); rec.Code != http.StatusSeeOther || e.config(t, ep.ID).Forward != nil {
		t.Fatalf("remove: %d", rec.Code)
	}
}

// TestReceiverNeedsAdmin is AC-8 for the UI: a user sees the receiver but
// cannot set, change or remove it; an admin can.
func TestReceiverNeedsAdmin(t *testing.T) {
	e := newAccessEnv(t, "")
	e.addUser(t, "ann", "admin", "admin-password-1234")
	e.addUser(t, "bob", "user", "user-password-5678")
	ann, bob := e.login(t, "ann", "admin-password-1234"), e.login(t, "bob", "user-password-5678")
	ep, _ := e.st.CreateEndpoint(context.Background(), "/live")
	base := "/admin/e/" + id(ep.ID) + "/behaviour"
	version := func() string {
		got, _ := e.st.Endpoint(context.Background(), ep.ID)
		return configVersion(got.Config)
	}
	set := url.Values{"op": {"receiver"}, "url": {"https://receiver.test/in"}}
	set.Set("v", version())
	rec := e.form(base, bob, set)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "only an admin can change where requests are forwarded") {
		t.Fatalf("a user sets it: %d %q", rec.Code, errorOf(rec.Body.String()))
	}
	if got, _ := e.st.Endpoint(context.Background(), ep.ID); strings.Contains(got.Config, "forward") {
		t.Fatal("stored after a refusal")
	}
	set.Set("v", version())
	if rec := e.form(base, ann, set); rec.Code != http.StatusSeeOther {
		t.Fatalf("an admin sets it: %d %q", rec.Code, errorOf(rec.Body.String()))
	}
	page := e.req("GET", base+"?tab=receiver", bob, "", nil).Body.String()
	if !strings.Contains(page, "Only an admin can change where requests are forwarded.") || strings.Contains(page, "Remove receiver") ||
		strings.Contains(page, ">Edit receiver</a>") || !strings.Contains(page, "<code>https://receiver.test/in</code>") {
		t.Fatal("a user's receiver tab")
	}
	// A user's other edits stand; removing it does not.
	if rec := e.form(base, bob, url.Values{"op": {"default"}, "status": {"204"}, "v": {version()}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("a user edits the default: %d %q", rec.Code, errorOf(rec.Body.String()))
	}
	if rec := e.form(base, bob, url.Values{"op": {"receiver-del"}, "v": {version()}}); rec.Code != http.StatusForbidden {
		t.Fatalf("a user removes it: %d", rec.Code)
	}
}

// TestReceiverOnTheRegister: the badge on the endpoint list and register,
// the row marks, and the request page's Receiver section.
func TestReceiverOnTheRegister(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, _ := e.st.CreateEndpoint(ctx, "/live")
	e.st.SetConfig(ctx, ep.ID, emulate.Config{Default: emulate.Reply{Status: 200}, Forward: &emulate.Forward{URL: "https://receiver.test/in"}}.String())
	add := func(outcome string, status int) *store.Request {
		r := &store.Request{EndpointID: ep.ID, ReceivedAt: time.Now(), Method: "POST", Path: "/live", Proto: "HTTP/1.1", Body: []byte("{}"),
			Size: 2, Status: status, Outcome: outcome}
		if err := e.st.AddRequest(ctx, r, store.Keep{Normal: 100, Anomalies: 100}, nil); err != nil {
			t.Fatal(err)
		}
		return r
	}
	ok := add(`{"source":"forward","action":"forward","forward":{"target":"https://receiver.test/in","pending":true,"ms":0}}`, 0)
	e.st.FinishForward(ctx, &store.Forward{RequestID: ok.ID, CreatedAt: time.Now(), URL: "https://receiver.test/in", Options: `{"host":"target"}`,
		Line: "POST /in HTTP/1.1", Sent: []store.Header{{Name: "Host", Value: "receiver.test"}}, Status: 201, Reason: "Created",
		Headers: []store.Header{{Name: "Content-Type", Value: "application/json"}}, Body: []byte(`{"id":7}`), Size: 8, Duration: 38 * time.Millisecond},
		201, `{"source":"forward","action":"forward","status":201,"forward":{"target":"https://receiver.test/in","status":201,"ms":38}}`)
	add(`{"source":"forward","action":"forward","status":200,"forward":{"target":"https://receiver.test/in","error_kind":"dial","fallback":true,"ms":3}}`, 200)
	add(`{"source":"rule","rule":1,"label":"every 3rd","action":"reply","status":503,"not_forwarded":true}`, 503)
	if page := e.do("GET", "/admin/", nil).Body.String(); !strings.Contains(page, "→ https://receiver.test/in") {
		t.Fatal("no badge on the endpoint list")
	}
	reg := e.do("GET", "/admin/e/"+id(ep.ID), nil).Body.String()
	for _, want := range []string{"→ https://receiver.test/in", "201 from receiver", "200 default · receiver down", "not forwarded", `data-mark="bad"`} {
		if !strings.Contains(reg, want) {
			t.Errorf("the register lacks %q", want)
		}
	}
	ex := e.do("GET", "/admin/e/"+id(ep.ID)+"/r/"+id(ok.ID), nil).Body.String()
	for _, want := range []string{"The receiver&#39;s 201 was passed back", "<h2>Receiver</h2>", "201 Created", "https://receiver.test/in",
		"POST /in HTTP/1.1", "&#34;id&#34;", "/admin/r/" + id(ok.ID) + "/forward/body", "Host of the receiver"} {
		if !strings.Contains(ex, want) {
			t.Errorf("the request page lacks %q", want)
		}
	}
	if body := e.do("GET", "/admin/r/"+id(ok.ID)+"/forward/body", nil); body.Body.String() != `{"id":7}` || body.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("download %d %q", body.Code, body.Body)
	}
}

// The import review names each endpoint's receiver.
func TestImportShowsReceivers(t *testing.T) {
	e := newEnv(t)
	file := `{"hookwitness":2,"endpoints":[{"path":"/live","default":{"status":200},"forward":{"url":"https://receiver.test/in"},"rules":[],"checks":[],"vars":[],"schemas":[],"secrets":[]}]}`
	rec := e.do("POST", "/admin/import", url.Values{"op": {"review"}, "text": {file}})
	if !strings.Contains(rec.Body.String(), "Forwards live traffic to <code>https://receiver.test/in</code>") {
		t.Fatalf("review %d: %s", rec.Code, errorOf(rec.Body.String()))
	}
}

// A forward still pending long after it could have ended (the server
// stopped without recording it) is shown as interrupted.
func TestInterruptedForward(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, _ := e.st.CreateEndpoint(ctx, "/live")
	old := &store.Request{EndpointID: ep.ID, ReceivedAt: time.Now().Add(-10 * time.Minute), Method: "POST", Path: "/live", Proto: "HTTP/1.1",
		Outcome: `{"source":"forward","action":"forward","forward":{"target":"https://receiver.test/in","pending":true,"ms":0}}`}
	fresh := &store.Request{EndpointID: ep.ID, ReceivedAt: time.Now(), Method: "POST", Path: "/live", Proto: "HTTP/1.1", Outcome: old.Outcome}
	for _, r := range []*store.Request{old, fresh} {
		if err := e.st.AddRequest(ctx, r, store.Keep{Normal: 100, Anomalies: 100}, nil); err != nil {
			t.Fatal(err)
		}
	}
	reg := e.do("GET", "/admin/e/"+id(ep.ID), nil).Body.String()
	if !strings.Contains(reg, "interrupted: no answer recorded") || strings.Contains(reg, "0 receiver") || !strings.Contains(reg, "forwarding…") {
		t.Fatal("the register does not tell an interrupted forward from a running one")
	}
	if ex := e.do("GET", "/admin/e/"+id(ep.ID)+"/r/"+id(old.ID), nil).Body.String(); !strings.Contains(ex, "interrupted: the server stopped before the exchange ended") {
		t.Fatal("the request page")
	}
}

// AC-8 for the UI's import: a user's import that brings a receiver is
// refused and creates nothing; an admin's goes through.
func TestImportReceiverNeedsAdmin(t *testing.T) {
	e := newAccessEnv(t, "")
	e.addUser(t, "ann", "admin", "admin-password-1234")
	e.addUser(t, "bob", "user", "user-password-5678")
	ann, bob := e.login(t, "ann", "admin-password-1234"), e.login(t, "bob", "user-password-5678")
	file := `{"hookwitness":2,"endpoints":[{"path":"/live","default":{"status":200},"forward":{"url":"https://receiver.test/in"},"rules":[],"checks":[],"vars":[],"schemas":[],"secrets":[]}]}`
	f := url.Values{"op": {"import"}, "text": {file}, "path_0": {"/live"}}
	rec := e.form("/admin/import", bob, f)
	if !strings.Contains(rec.Body.String(), "only an admin can change where requests are forwarded") {
		t.Fatalf("a user's import: %d %q", rec.Code, rec.Body.String()[:min(300, rec.Body.Len())])
	}
	if _, _, _, err := e.st.EndpointByPath(context.Background(), "/live"); err != store.ErrNotFound {
		t.Fatal("a refused import created the endpoint")
	}
	if rec := e.form("/admin/import", ann, f); rec.Code != http.StatusSeeOther {
		t.Fatalf("an admin's import: %d", rec.Code)
	}
}
