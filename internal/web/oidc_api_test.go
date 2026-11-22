package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/oidc/oidctest"
	"github.com/petroladkin/hookwitness/internal/store"
)

// apiCall sends a JSON request with a browser's session.
func (e *oidcEnv) apiCall(t *testing.T, b *http.Client, method, path, body string) (int, map[string]any, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.srv.URL+"/admin/api/v1"+path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := b.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return res.StatusCode, out, string(raw)
}

func errCode(out map[string]any) (code, field string) {
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(string)
	f, _ := e["field"].(string)
	return c, f
}

func TestOIDCAPI(t *testing.T) {
	e := newOIDCEnv(t, "/admin")
	e.addUser(t, "ann", store.RoleAdmin, "ann@example.test")
	admin := e.passwordLogin(t, "ann")

	// Open, and nothing secret in it.
	code, _, raw := e.apiCall(t, browser(), "GET", "/login/providers", "")
	if code != 200 || strings.TrimSpace(strings.Join(strings.Fields(raw), "")) != `[{"label":"TestIdP","name":"test"}]` {
		t.Fatalf("providers: %d %s", code, raw)
	}

	code, out, raw := e.apiCall(t, admin, "POST", "/users", `{"name":"pat","email":"Pat@Example.test"}`)
	if code != 201 || out["email"] != "pat@example.test" || out["password"] != false {
		t.Fatalf("add a passwordless user: %d %s", code, raw)
	}
	if code, out, _ := e.apiCall(t, admin, "POST", "/users", `{"name":"quinn","email":"pat@example.test"}`); code != 409 {
		t.Fatalf("a taken email: %d %v", code, out)
	} else if c, f := errCode(out); c != "email_taken" || f != "email" {
		t.Fatalf("a taken email: %s %s", c, f)
	}
	if code, out, _ := e.apiCall(t, admin, "POST", "/users", `{"name":"quinn"}`); code != 422 {
		t.Fatalf("no password, no email: %d", code)
	} else if _, f := errCode(out); f != "password" {
		t.Fatalf("no password, no email: field %s", f)
	}
	for body, want := range map[string]string{`{"email":"no-at"}`: "invalid", `{"email":"ANN@example.test"}`: "email_taken", `{"email":12}`: "invalid"} {
		code, out, _ := e.apiCall(t, admin, "PATCH", "/users/pat", body)
		if c, f := errCode(out); c != want || f != "email" || (code != 422 && code != 409) {
			t.Errorf("PATCH %s: %d %s %s", body, code, c, f)
		}
	}
	if code, out, _ := e.apiCall(t, admin, "PATCH", "/users/pat", `{"email":null}`); code != 200 || out["email"] != nil {
		t.Fatalf("clear an email: %d %v", code, out)
	}
	e.apiCall(t, admin, "PATCH", "/users/pat", `{"email":"pat@example.test"}`)

	// pat signs in; the identity shows in /users and pat's /me.
	e.idp.Next(oidctest.Claims{Subject: "s-pat", Email: "pat@example.test", EmailVerified: oidctest.Ptr(true)})
	pat := browser()
	e.signIn(t, pat, "/admin/")
	_, _, raw = e.apiCall(t, admin, "GET", "/users", "")
	var users struct {
		Users []struct {
			Name       string
			Email      *string
			Password   bool
			Identities []map[string]any
		}
	}
	json.Unmarshal([]byte(raw), &users)
	found := false
	for _, u := range users.Users {
		if u.Name == "pat" {
			found = len(u.Identities) == 1 && u.Identities[0]["provider"] == "test" && u.Identities[0]["label"] == "TestIdP" &&
				u.Identities[0]["issuer"] == e.idp.URL && !u.Password
		}
	}
	if !found {
		t.Fatalf("pat in /users: %s", raw)
	}
	code, out, raw = e.apiCall(t, pat, "GET", "/me", "")
	if code != 200 || out["email"] != "pat@example.test" || out["password"] != false || len(out["identities"].([]any)) != 1 {
		t.Fatalf("pat's /me: %s", raw)
	}
	// The only way in: refused.
	if code, out, _ := e.apiCall(t, pat, "DELETE", "/me/identities/test", ""); code != 409 {
		t.Fatalf("own last way in: %d", code)
	} else if c, _ := errCode(out); c != "last_way_in" {
		t.Fatalf("own last way in: %s", c)
	}
	// A first password: refused from a session past the window, then set.
	window := access.FirstPasswordWindow
	access.FirstPasswordWindow = time.Nanosecond
	time.Sleep(time.Millisecond)
	if code, out, _ := e.apiCall(t, pat, "PUT", "/me/password", `{"new":"a-first-password-0001"}`); code != 422 {
		t.Fatalf("a first password from an old session: %d", code)
	} else if c, f := errCode(out); c != "reauth" || f != "current" {
		t.Fatalf("a first password from an old session: %s %s", c, f)
	}
	access.FirstPasswordWindow = window
	if code, _, raw := e.apiCall(t, pat, "PUT", "/me/password", `{"new":"a-first-password-0001"}`); code != 204 {
		t.Fatalf("a first password: %d %s", code, raw)
	}
	// An admin unlinks pat: pat's logins end.
	if code, _, _ := e.apiCall(t, admin, "DELETE", "/users/pat/identities/nope", ""); code != 404 {
		t.Fatalf("an unknown provider: %d", code)
	}
	if code, _, raw := e.apiCall(t, admin, "DELETE", "/users/pat/identities/test", ""); code != 204 {
		t.Fatalf("unlink pat: %d %s", code, raw)
	}
	if code, _, _ := e.apiCall(t, pat, "GET", "/me", ""); code != 401 {
		t.Fatalf("pat's session after the unlink: %d", code)
	}
	// An identity whose provider is no longer configured: by ?issuer=.
	if _, err := e.st.SignIn(context.Background(), store.SignInInput{Issuer: "https://gone.example.test", Subject: "g", Provider: "gone", Email: "pat@example.test", Verified: true}); err != nil {
		t.Fatal(err)
	}
	if code, _, raw := e.apiCall(t, admin, "DELETE", "/users/pat/identities/gone?issuer=https://gone.example.test", ""); code != 204 {
		t.Fatalf("unlink by issuer: %d %s", code, raw)
	}
}
