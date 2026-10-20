package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/live"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
)

type usersEnv struct {
	env
	auth *access.Authenticator
}

func newUsersEnv(t *testing.T) usersEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "hw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	keys, err := secret.Load("", filepath.Join(dir, "hw.db.key"))
	if err != nil {
		t.Fatal(err)
	}
	auth := access.New(st, keys, "")
	logs := &syncBuffer{}
	stop := make(chan struct{})
	h := New(st, live.New(), slog.New(slog.NewTextHandler(logs, nil)),
		Options{Prefix: "/admin", Reserved: "/admin", MaxWait: time.Minute, Keys: keys, Keyring: keys, Stop: stop, Access: auth})
	return usersEnv{env{h, st, nil, logs, stop}, auth}
}

// as is a context acting as the user name through a session (or via).
func (e usersEnv) as(t *testing.T, name string, via access.Via) context.Context {
	t.Helper()
	u, err := e.st.UserByName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return access.With(context.Background(), &access.Identity{UserID: u.ID, Name: u.Name, Role: u.Role, Via: via, SessionID: -1})
}

func TestUsersAPI(t *testing.T) {
	e := newUsersEnv(t)
	// Login off: user management is open (to add the first admin); own
	// tokens have no owner.
	wantError(t, e.do("GET", "/me/tokens", ""), http.StatusConflict, "login_off", "")
	wantError(t, e.do("POST", "/users", `{"name":"ann","role":"user","password":"admin-password-1234"}`), 422, "invalid", "role")
	wantError(t, e.do("POST", "/users", `{"name":"a b","role":"admin","password":"admin-password-1234"}`), 422, "invalid", "name")
	wantError(t, e.do("POST", "/users", `{"name":"ann","role":"admin","password":"short"}`), 422, "invalid", "password")
	wantError(t, e.do("POST", "/users", `{"name":"ann","role":"boss","password":"admin-password-1234"}`), 422, "invalid", "role")
	if rec := e.do("POST", "/users", `{"name":"Ann","role":"admin","password":"admin-password-1234"}`); rec.Code != 201 ||
		!strings.Contains(rec.Body.String(), `"name": "ann"`) || strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("first admin: %d %s", rec.Code, rec.Body)
	}
	// Login on now.
	wantError(t, e.do("GET", "/users", ""), 401, "unauthorized", "")
	ann := e.as(t, "ann", access.ViaSession)
	wantError(t, e.call(ann, "POST", "/users", `{"name":"ANN","role":"user","password":"user-password-5678"}`), 409, "exists", "name")
	if rec := e.call(ann, "POST", "/users", `{"name":"bob","password":"user-password-5678"}`); rec.Code != 201 || !strings.Contains(rec.Body.String(), `"role": "user"`) {
		t.Fatalf("add bob: %d %s", rec.Code, rec.Body)
	}
	wantError(t, e.call(ann, "DELETE", "/users/ann", ""), 409, "last_admin", "")
	// Demoting herself is refused before the last-admin guard is reached.
	wantError(t, e.call(ann, "PATCH", "/users/ann", `{"role":"user"}`), 409, "self_demotion", "")
	wantError(t, e.call(ann, "PATCH", "/users/nobody", `{"role":"user"}`), 404, "not_found", "")
	wantError(t, e.call(ann, "PATCH", "/users/bob", `{"password":"short"}`), 422, "invalid", "password")
	// The singular path of the first build is gone.
	if rec := e.call(ann, "PATCH", "/user/bob", `{"role":"admin"}`); rec.Code != 404 {
		t.Fatalf("PATCH /user/bob: %d", rec.Code)
	}

	bob := e.as(t, "bob", access.ViaSession)
	wantError(t, e.call(bob, "GET", "/users", ""), 403, "forbidden", "")
	wantError(t, e.call(bob, "GET", "/tokens", ""), 403, "forbidden", "")
	rec := e.call(bob, "POST", "/me/tokens", `{"name":"ci"}`)
	if rec.Code != 201 {
		t.Fatalf("make a token: %d %s", rec.Code, rec.Body)
	}
	made := decodeAs[map[string]any](t, rec)
	tok, _ := made["token"].(string)
	if !access.TokenRE().MatchString(tok) || made["shown"] != access.Shown(made["id"].(string)) {
		t.Fatalf("made %v", made)
	}
	wantError(t, e.call(bob, "POST", "/me/tokens", `{"name":"ci"}`), 409, "exists", "name")
	if rec := e.call(bob, "GET", "/me/tokens", ""); strings.Contains(rec.Body.String(), tok) || !strings.Contains(rec.Body.String(), `"name": "ci"`) {
		t.Fatalf("token list shows the value or not the token: %s", rec.Body)
	}
	if rec := e.call(ann, "GET", "/tokens", ""); strings.Contains(rec.Body.String(), tok) || !strings.Contains(rec.Body.String(), `"owner": "bob"`) {
		t.Fatalf("everyone's tokens: %s", rec.Body)
	}
	// A token cannot manage tokens or users, whoever owns it.
	bobTok := e.as(t, "bob", access.ViaToken)
	wantError(t, e.call(bobTok, "POST", "/me/tokens", `{"name":"more"}`), 403, "session_required", "")
	annTok := e.as(t, "ann", access.ViaToken)
	wantError(t, e.call(annTok, "GET", "/users", ""), 403, "session_required", "")
	if rec := e.call(bobTok, "GET", "/me", ""); !strings.Contains(rec.Body.String(), `"via": "token"`) {
		t.Fatalf("me: %s", rec.Body)
	}
	// Bob cannot revoke Ann's tokens; Ann can revoke Bob's.
	annT, at, _ := e.auth.MintFor(context.Background(), mustID(t, e, "ann"), "x")
	_ = annT
	wantError(t, e.call(bob, "DELETE", "/me/tokens/"+at.PublicID, ""), 404, "not_found", "")
	if rec := e.call(ann, "DELETE", "/tokens/"+made["id"].(string), ""); rec.Code != 204 {
		t.Fatalf("admin revokes: %d", rec.Code)
	}
	// Own password: needs the current one.
	wantError(t, e.call(bob, "PUT", "/me/password", `{"current":"wrong-password-000","new":"bob-new-password-1"}`), 422, "invalid", "current")
	wantError(t, e.call(bob, "PUT", "/me/password", `{"current":"user-password-5678","new":"short"}`), 422, "invalid", "new")
	if rec := e.call(bob, "PUT", "/me/password", `{"current":"user-password-5678","new":"bob-new-password-1"}`); rec.Code != 204 {
		t.Fatalf("change own password: %d %s", rec.Code, rec.Body)
	}
	if _, err := e.auth.Login(context.Background(), "bob", "bob-new-password-1"); err != nil {
		t.Fatalf("new password: %v", err)
	}
	if rec := e.call(ann, "PATCH", "/users/bob", `{"role":"admin"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"role": "admin"`) {
		t.Fatalf("promote: %d %s", rec.Code, rec.Body)
	}
	// With another admin there she still cannot demote herself; bob can.
	wantError(t, e.call(ann, "PATCH", "/users/ann", `{"role":"user"}`), 409, "self_demotion", "")
	bobAdmin := e.as(t, "bob", access.ViaSession)
	if rec := e.call(bobAdmin, "PATCH", "/users/ann", `{"role":"user"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"role": "user"`) {
		t.Fatalf("bob demotes ann: %d %s", rec.Code, rec.Body)
	}
	if rec := e.call(bobAdmin, "PATCH", "/users/ann", `{"role":"admin"}`); rec.Code != 200 {
		t.Fatalf("bob promotes ann back: %d %s", rec.Code, rec.Body)
	}
	if rec := e.call(ann, "DELETE", "/users/ann", ""); rec.Code != 204 {
		t.Fatalf("ann removes herself with another admin there: %d", rec.Code)
	}
	for _, line := range []string{`user added" name=bob role=user user=ann`, `token created" token=ci`, `password changed" user=bob`, `user removed" name=ann user=ann`} {
		if !strings.Contains(e.log.String(), line) {
			t.Errorf("log lacks %q:\n%s", line, e.log.String())
		}
	}
}

func mustID(t *testing.T, e usersEnv, name string) int64 {
	t.Helper()
	u, err := e.st.UserByName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// TestAccessAnswersMatchTheirSchemas: the user, token and caller answers
// validate against the document.
func TestAccessAnswersMatchTheirSchemas(t *testing.T) {
	c := compiler(t)
	e := newUsersEnv(t)
	e.do("POST", "/users", `{"name":"ann","role":"admin","password":"admin-password-1234"}`)
	ann := e.as(t, "ann", access.ViaSession)
	made := e.call(ann, "POST", "/me/tokens", `{"name":"ci"}`)
	for _, tc := range []struct {
		rec    *httptest.ResponseRecorder
		schema string
	}{
		{made, "#/components/schemas/Token"},
		{e.call(ann, "GET", "/me", ""), "#/components/schemas/Me"},
		{e.do("GET", "/me", ""), "#/components/schemas/Error"}, // login on, no credentials
		{e.call(ann, "GET", "/users", ""), "#/paths/~1users/get/responses/200/content/application~1json/schema"},
		{e.call(ann, "GET", "/me/tokens", ""), "#/paths/~1me~1tokens/get/responses/200/content/application~1json/schema"},
		{e.call(ann, "GET", "/tokens", ""), "#/paths/~1tokens/get/responses/200/content/application~1json/schema"},
		{e.call(ann, "POST", "/users", `{"name":"bob","password":"user-password-5678"}`), "#/components/schemas/User"},
		{e.call(ann, "DELETE", "/users/ann", ""), "#/components/schemas/Error"},
	} {
		sch, err := c.Compile(docURL + tc.schema)
		if err != nil {
			t.Fatal(err)
		}
		v, err := jsonschema.UnmarshalJSON(bytes.NewReader(tc.rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("%s: %v (%s)", tc.schema, err, tc.rec.Body)
		}
		if err := sch.Validate(v); err != nil {
			t.Errorf("%d %s does not match %s: %v", tc.rec.Code, tc.rec.Body, tc.schema, err)
		}
	}
	if rec := e.do("GET", "/me", ""); rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != `Bearer realm="hookwitness"` {
		t.Fatalf("401 without WWW-Authenticate: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}
