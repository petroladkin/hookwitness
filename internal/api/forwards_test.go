package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/store"
)

const fwBody = `{"default":{"status":202},"forward":{"url":"https://receiver.test/in","timeout_ms":5000},
	"rules":[{"when":{"kind":"every","n":3},"do":"reply","reply":{"status":503},"forward":true}]}`

// TestBehaviourForward: the receiver is read and replaced with the rest of
// the behaviour, its URL and timeout are checked at save, and the answer
// warns of an open relay until an enforced authorization check stands.
func TestBehaviourForward(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	_, tag := e.behaviour(t, ep.ID)
	rec := e.putBehaviour(ep.ID, tag, fwBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	got := decodeAs[behaviourJSON](t, rec)
	if got.Forward == nil || got.Forward.URL != "https://receiver.test/in" || !got.Rules[0].Forward || len(got.Warnings) != 1 ||
		got.Warnings[0] != OpenRelay {
		t.Fatalf("answer %+v", got)
	}
	if cfg := e.stored(t, ep.ID); cfg.Forward == nil || cfg.Forward.TimeoutMs != 5000 {
		t.Fatalf("stored %+v", cfg.Forward)
	}
	if v := decodeAs[endpointJSON](t, e.do("GET", fmt.Sprintf("/endpoints/%d", ep.ID), "")); v.ForwardsTo != "https://receiver.test/in" {
		t.Fatalf("forwards_to %q", v.ForwardsTo)
	}
	for body, msg := range map[string]string{
		`{"forward":{"url":"https://receiver.test/in?token=1"}}`:             "must not have a query",
		`{"forward":{"url":"https://u:p@receiver.test/in"}}`:                 "user name or password",
		`{"forward":{"url":"https://receiver.test/in","timeout_ms":50}}`:     "from 100 ms to 60000 ms",
		`{"forward":{"url":"https://receiver.test/in","on_failure":"echo"}}`: "on_failure",
		`{"forward":{"url":"http://169.254.169.254/latest"}}`:                "link-local",
		`{"forward":{"url":"http://[fd00:ec2::254]/"}}`:                      "metadata",
		`{"forward":{"url":"https://receiver.test/in","color":1}}`:           "",
	} {
		_, tag := e.behaviour(t, ep.ID)
		rec := e.putBehaviour(ep.ID, tag, body)
		if msg == "" {
			wantError(t, rec, http.StatusBadRequest, "unknown_field", "color")
			continue
		}
		got := wantError(t, rec, http.StatusUnprocessableEntity, "invalid", "forward")
		if !strings.Contains(got.Error.Message, msg) || !strings.HasPrefix(got.Error.Message, "receiver: ") {
			t.Errorf("%s: %q", body, got.Error.Message)
		}
	}
	// An enforced signature check closes the relay.
	e.do("PUT", fmt.Sprintf("/endpoints/%d/secrets/GH", ep.ID), `{"kind":"key","value":"gh_test"}`)
	_, tag = e.behaviour(t, ep.ID)
	rec = e.putBehaviour(ep.ID, tag, `{"forward":{"url":"https://receiver.test/in"},"checks":[{"kind":"github","secret":"GH","enforce":true}]}`)
	if got := decodeAs[behaviourJSON](t, rec); rec.Code != 200 || len(got.Warnings) != 0 || strings.Contains(rec.Body.String(), "warnings") {
		t.Fatalf("with an enforced check: %d %s", rec.Code, rec.Body)
	}
	// Removing it.
	_, tag = e.behaviour(t, ep.ID)
	if rec := e.putBehaviour(ep.ID, tag, `{}`); rec.Code != 200 || e.stored(t, ep.ID).Forward != nil {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
}

// TestForwardNeedsAdmin is AC-8 for the API: once login is on, a user
// cannot add, change or remove a receiver (the stored behaviour unchanged)
// but may edit the rest by sending the stored one unchanged; an admin may.
func TestForwardNeedsAdmin(t *testing.T) {
	e := newUsersEnv(t)
	ep := e.endpoint(t, "/hooks")
	e.do("POST", "/users", `{"name":"ann","role":"admin","password":"admin-password-1234"}`)
	ann := e.as(t, "ann", access.ViaSession)
	e.call(ann, "POST", "/users", `{"name":"bob","password":"user-password-5678"}`)
	bob, bobTok := e.as(t, "bob", access.ViaSession), e.as(t, "bob", access.ViaToken)
	path := fmt.Sprintf("/endpoints/%d/behaviour", ep.ID)
	tagOf := func() string { return e.call(ann, "GET", path, "").Header().Get("ETag") }
	for _, who := range []context.Context{bob, bobTok} {
		got := wantError(t, e.call(who, "PUT", path, fwBody, "If-Match", tagOf()), http.StatusForbidden, "forbidden", "forward")
		if !strings.Contains(got.Error.Message, "only an admin") || e.stored(t, ep.ID).Forward != nil {
			t.Fatalf("a user adds a receiver: %q", got.Error.Message)
		}
	}
	if rec := e.call(ann, "PUT", path, fwBody, "If-Match", tagOf()); rec.Code != 200 {
		t.Fatalf("an admin adds: %d %s", rec.Code, rec.Body)
	}
	// The user edits the rest, sending the receiver back as read.
	read := e.call(bob, "GET", path, "")
	sent := strings.Replace(read.Body.String(), `"status": 202`, `"status": 204`, 1)
	if rec := e.call(bob, "PUT", path, sent, "If-Match", read.Header().Get("ETag")); rec.Code != 200 || e.stored(t, ep.ID).Default.Status != 204 {
		t.Fatalf("a user keeps the receiver: %d %s", rec.Code, rec.Body)
	}
	for _, body := range []string{`{"default":{"status":200}}`, strings.Replace(fwBody, "receiver.test", "elsewhere.test", 1),
		strings.Replace(fwBody, `"timeout_ms":5000`, `"timeout_ms":6000`, 1)} {
		before := e.stored(t, ep.ID).String()
		wantError(t, e.call(bob, "PUT", path, body, "If-Match", tagOf()), http.StatusForbidden, "forbidden", "forward")
		if e.stored(t, ep.ID).String() != before {
			t.Fatal("a refused PUT changed the behaviour")
		}
	}
	// A setup import that brings a receiver: admin only.
	file := `{"hookwitness":2,"endpoints":[{"path":"/new","default":{"status":200},"forward":{"url":"https://receiver.test/x"}}]}`
	got := wantError(t, e.call(bob, "POST", "/setup", file), http.StatusForbidden, "forbidden", "")
	if !strings.Contains(got.Error.Message, "/new: only an admin") {
		t.Fatalf("import by a user: %q", got.Error.Message)
	}
	if _, _, _, err := e.st.EndpointByPath(context.Background(), "/new"); err != store.ErrNotFound {
		t.Fatal("a refused import created the endpoint")
	}
	if rec := e.call(ann, "POST", "/setup", file); rec.Code != 201 {
		t.Fatalf("import by an admin: %d %s", rec.Code, rec.Body)
	}
	// Re-importing the same receiver changes nothing: a user may.
	if rec := e.call(bob, "POST", "/setup?on_conflict=replace", file); rec.Code != 200 {
		t.Fatalf("a user re-imports it unchanged: %d %s", rec.Code, rec.Body)
	}
	bad := strings.Replace(file, "receiver.test/x", "169.254.169.254/x", 1)
	if rec := e.call(ann, "POST", "/setup?on_conflict=replace", bad); rec.Code != 422 || !strings.Contains(rec.Body.String(), "link-local") {
		t.Fatalf("a refused target: %d %s", rec.Code, rec.Body)
	}
}

// TestRequestForward: a forwarded request shows its receiver's side; the
// whole exchange and its body have routes; lists filter by it.
func TestRequestForward(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	ok := e.add(t, ep.ID, "{}", func(r *store.Request) {
		r.Outcome = `{"source":"forward","action":"forward","status":201,"forward":{"target":"https://receiver.test/in","status":201,"ms":38}}`
		r.Status = 201
	})
	down := e.add(t, ep.ID, "{}", func(r *store.Request) {
		r.Outcome = `{"source":"forward","action":"forward","status":502,"forward":{"target":"https://receiver.test/in","error_kind":"dial","fallback":true,"ms":3}}`
		r.Status = 502
	})
	kept := e.add(t, ep.ID, "{}", func(r *store.Request) {
		r.Outcome = `{"source":"rule","rule":1,"label":"every 3rd","action":"reply","status":503,"not_forwarded":true}`
		r.Status = 503
	})
	if err := e.st.FinishForward(context.Background(), &store.Forward{RequestID: ok.ID, CreatedAt: time.Now(), URL: "https://receiver.test/in",
		Options: `{"host":"target","insecure_tls":false,"timeout_ms":30000}`, Line: "POST /in HTTP/1.1", Sent: []store.Header{{Name: "Host", Value: "receiver.test"}},
		Proto: "HTTP/1.1", Status: 201, Reason: "Created", Headers: []store.Header{{Name: "content-type", Value: "text/plain"}}, Body: []byte("made"), Size: 4,
		Duration: 38 * time.Millisecond}, 201, `{"source":"forward","action":"forward","status":201,"forward":{"target":"https://receiver.test/in","status":201,"ms":38}}`); err != nil {
		t.Fatal(err)
	}
	r := decodeAs[requestJSON](t, e.do("GET", fmt.Sprintf("/requests/%d", ok.ID), ""))
	if r.Forward == nil || r.Forward.Status != 201 || r.Forward.Ms != 38 || r.NotForwarded {
		t.Fatalf("request %+v", r.summaryJSON)
	}
	fw := decodeAs[forwardJSON](t, e.do("GET", fmt.Sprintf("/requests/%d/forward", ok.ID), ""))
	if fw.Status != 201 || fw.Reason != "Created" || fw.Host != "target" || *fw.BodyText != "made" || fw.Headers[0][0] != "content-type" || fw.Error != nil {
		t.Fatalf("exchange %+v", fw)
	}
	body := e.do("GET", fmt.Sprintf("/requests/%d/forward/body", ok.ID), "")
	if body.Body.String() != "made" || body.Header().Get("Content-Type") != "text/plain" || body.Header().Get("Content-Security-Policy") != "sandbox; default-src 'none'" {
		t.Fatalf("body %d %v %q", body.Code, body.Header(), body.Body)
	}
	wantError(t, e.do("GET", fmt.Sprintf("/requests/%d/forward", kept.ID), ""), 404, "not_found", "")
	for filter, want := range map[string][]int64{"ok": {ok.ID}, "failed": {down.ID}, "none": {kept.ID}} {
		l := e.list(t, "?forward="+filter)
		if len(l.Requests) != len(want) || l.Requests[0].ID != want[0] {
			t.Errorf("forward=%s: %+v", filter, l.Requests)
		}
	}
	if l := e.list(t, "?forward=none"); !l.Requests[0].NotForwarded {
		t.Fatalf("not forwarded: %+v", l.Requests[0])
	}
	wantError(t, e.do("GET", fmt.Sprintf("/endpoints/%d/requests?forward=maybe", ep.ID), ""), 422, "invalid", "forward")
}
