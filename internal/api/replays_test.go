package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/store"
)

func TestReplayAPI(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	var seen struct {
		host, uri, sig string
		body           []byte
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.host, seen.uri, seen.sig = r.Host, r.RequestURI, r.Header.Get("X-Signature")
		seen.body, _ = io.ReadAll(r.Body)
		w.Header()["x-lower"] = []string{"1"}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "signature mismatch")
	}))
	defer target.Close()
	r := e.add(t, ep.ID, `{"a":1}`, func(r *store.Request) {
		r.Path = "/hooks"
		r.Headers = []store.Header{{Name: "Host", Value: "hw.test"}, {Name: "X-Signature", Value: "abc"}, {Name: "Content-Length", Value: "7"}}
	})
	base := fmt.Sprintf("/requests/%d/replays", r.ID)

	rec := e.do("POST", base, `{"url":"`+target.URL+`/webhooks?x=1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body)
	}
	rp := decodeAs[replayJSON](t, rec)
	if rp.Status != 401 || rp.BodyText == nil || *rp.BodyText != "signature mismatch" || rp.Error != nil || rp.Host != "target" ||
		rp.TimeoutMS != 30000 || rp.Line != "POST /webhooks?x=1 HTTP/1.1" || rp.RequestID != r.ID {
		t.Fatalf("replay %+v", rp)
	}
	if seen.host != strings.TrimPrefix(target.URL, "http://") || seen.uri != "/webhooks?x=1" || seen.sig != "abc" || string(seen.body) != `{"a":1}` {
		t.Fatalf("the target saw %+v", seen)
	}
	if rec := e.do("GET", fmt.Sprintf("/requests/%d", r.ID), ""); !strings.Contains(rec.Body.String(), `"replays": 1`) {
		t.Fatalf("request: %s", rec.Body)
	}
	if rec := e.do("GET", fmt.Sprintf("/endpoints/%d/requests", ep.ID), ""); !strings.Contains(rec.Body.String(), `"replays": 1`) {
		t.Fatalf("list: %s", rec.Body)
	}
	list := decodeAs[struct{ Replays []replayJSON }](t, e.do("GET", base, ""))
	if len(list.Replays) != 1 || list.Replays[0].BodyText != nil {
		t.Fatalf("list without bodies: %+v", list)
	}
	list = decodeAs[struct{ Replays []replayJSON }](t, e.do("GET", base+"?include=body", ""))
	if len(list.Replays) != 1 || list.Replays[0].BodyText == nil {
		t.Fatalf("list with bodies: %+v", list)
	}
	rec = e.do("GET", fmt.Sprintf("/replays/%d/body", rp.ID), "")
	if rec.Body.String() != "signature mismatch" || rec.Header().Get("Content-Type") != "text/plain" ||
		rec.Header().Get("Content-Security-Policy") != "sandbox; default-src 'none'" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("body: %v %s", rec.Header(), rec.Body)
	}
	// The received Host, timeout and TLS switch are recorded.
	rp = decodeAs[replayJSON](t, e.do("POST", base, `{"url":"`+target.URL+`/","host":"received","timeout_ms":2000,"insecure_tls":true}`))
	if seen.host != "hw.test" || rp.Host != "received" || rp.TimeoutMS != 2000 || !rp.InsecureTLS {
		t.Fatalf("switches: %+v, host seen %q", rp, seen.host)
	}

	// A target that does not answer is still a replay.
	rp = decodeAs[replayJSON](t, e.do("POST", base, `{"url":"http://127.0.0.1:1/"}`))
	if rp.Status != 0 || rp.Error == nil || rp.Error.Kind != "dial" {
		t.Fatalf("closed port: %+v", rp)
	}

	// Refusals.
	wantError(t, e.do("POST", base, `{}`), 422, "invalid", "url")
	wantError(t, e.do("POST", base, `{"url":"ftp://x/"}`), 422, "invalid", "url")
	wantError(t, e.do("POST", base, `{"url":"http://user:pw@127.0.0.1/"}`), 422, "invalid", "url")
	wantError(t, e.do("POST", base, `{"url":"http://127.0.0.1/","host":"mine"}`), 422, "invalid", "host")
	wantError(t, e.do("POST", base, `{"url":"http://127.0.0.1/","timeout_ms":3600000}`), 422, "invalid", "timeout_ms")
	wantError(t, e.do("POST", base, `{"url":"http://127.0.0.1/","headers":[]}`), 400, "unknown_field", "headers")
	ref := wantError(t, e.do("POST", base, `{"url":"http://169.254.169.254/latest/meta-data/"}`), 422, "target_refused", "url")
	if !strings.Contains(ref.Error.Message, "link-local") {
		t.Fatalf("refusal names no rule: %s", ref.Error.Message)
	}
	wantError(t, e.do("POST", "/requests/999/replays", `{"url":"http://127.0.0.1:1/"}`), 404, "not_found", "")
	wantError(t, e.do("GET", "/requests/999/replays", ""), 404, "not_found", "")
	wantError(t, e.do("GET", "/replays/999", ""), 404, "not_found", "")
	wantError(t, e.do("GET", base+"?include=all", ""), 422, "invalid", "include")
	big := e.add(t, ep.ID, "", func(r *store.Request) { r.TooLarge, r.Size, r.Body = true, 50<<20, nil })
	wantError(t, e.do("POST", fmt.Sprintf("/requests/%d/replays", big.ID), `{"url":"http://127.0.0.1:1/"}`), 409, "body_not_kept", "")
	if strings.Contains(e.log.String(), "meta-data") || strings.Contains(e.log.String(), "x=1") {
		t.Fatalf("the log has a target's path or query:\n%s", e.log)
	}

	// Every answer matches its schema.
	c := compiler(t)
	for _, tc := range []struct{ method, path, body, schema string }{
		{"POST", base, `{"url":"` + target.URL + `/"}`, "Replay"},
		{"GET", base, "", "ReplayList"},
		{"GET", base + "?include=body", "", "ReplayList"},
		{"GET", fmt.Sprintf("/replays/%d", rp.ID), "", "Replay"},
		{"GET", fmt.Sprintf("/requests/%d", r.ID), "", "Request"},
		{"POST", base, `{"url":"ftp://x/"}`, "Error"},
	} {
		sch, err := c.Compile(docURL + "#/components/schemas/" + tc.schema)
		if err != nil {
			t.Fatal(err)
		}
		rec := e.do(tc.method, tc.path, tc.body)
		v, err := jsonschema.UnmarshalJSON(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		if err := sch.Validate(v); err != nil {
			t.Errorf("%s %s (%d) does not match %s: %v", tc.method, tc.path, rec.Code, tc.schema, err)
		}
	}
}

// With login on, only a logged-in user or a token may replay, and the
// replay records who did.
func TestReplayNeedsLogin(t *testing.T) {
	e := newUsersEnv(t)
	ctx := context.Background()
	ep, _ := e.st.CreateEndpoint(ctx, "/a")
	r := &store.Request{EndpointID: ep.ID, Method: "POST", Path: "/a", Proto: "HTTP/1.1", Body: []byte("{}"), Size: 2,
		Headers: []store.Header{{Name: "Content-Length", Value: "2"}}}
	if err := e.st.AddRequest(ctx, r, 100, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.AddUser(ctx, "ann", "admin", e.auth.HashPassword("admin-password-1234")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.AddUser(ctx, "bob", "user", e.auth.HashPassword("user-password-1234")); err != nil {
		t.Fatal(err)
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer target.Close()
	path := fmt.Sprintf("/requests/%d/replays", r.ID)
	body := `{"url":"` + target.URL + `/"}`
	wantError(t, e.do("POST", path, body), 401, "unauthorized", "")
	wantError(t, e.do("GET", path, ""), 401, "unauthorized", "")
	for _, who := range []context.Context{e.as(t, "bob", access.ViaSession), e.as(t, "bob", access.ViaToken)} {
		rec := e.call(who, "POST", path, body)
		if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"by": "bob"`) {
			t.Fatalf("bob: %d %s", rec.Code, rec.Body)
		}
	}
	if !strings.Contains(e.log.String(), "user=bob") {
		t.Fatalf("the log does not name the user:\n%s", e.log)
	}
}
