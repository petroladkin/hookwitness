package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/live"
	"github.com/petroladkin/hookwitness/internal/oidc"
	"github.com/petroladkin/hookwitness/internal/oidc/oidctest"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
)

const (
	oidcClient = "hookwitness-test"
	oidcSecret = "client-secret-of-the-test"
)

// oidcEnv is the admin on a real listener (its URL is the admin's origin)
// with one provider, the test IdP.
type oidcEnv struct {
	srv  *httptest.Server
	h    *Handler
	st   *store.Store
	auth *access.Authenticator
	idp  *oidctest.Provider
	log  *lockedBuffer
	pre  string // the admin prefix
	db   string // the database file
}

func newOIDCEnv(t *testing.T, prefix string, domains ...string) *oidcEnv {
	t.Helper()
	old := loginAttempt
	loginAttempt = 0
	t.Cleanup(func() { loginAttempt = old })
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "hw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	keys, _ := secret.Load("", filepath.Join(dir, "hw.db.key"))
	auth := access.New(st, keys, "")
	idp := oidctest.New(oidcClient, oidcSecret)
	t.Cleanup(idp.Close)
	e := &oidcEnv{st: st, auth: auth, idp: idp, log: &lockedBuffer{}, pre: prefix, db: filepath.Join(dir, "hw.db")}
	var inner http.Handler
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { inner.ServeHTTP(w, r) }))
	t.Cleanup(e.srv.Close)
	p := oidc.NewProvider(oidc.Config{Name: "test", Label: "TestIdP", Issuer: idp.URL, ClientID: oidcClient, ClientSecret: oidcSecret,
		RedirectURI: e.srv.URL + prefix + "/oidc/test/callback", AllowedDomains: domains})
	e.h = New(st, live.New(), slog.New(slog.NewTextHandler(e.log, nil)), Options{Prefix: prefix, Separate: prefix == "", MaxBody: 1 << 20,
		Retention: store.Keep{Normal: 1000}, Keys: keys, Access: auth, OIDC: []*oidc.Provider{p}, AdminURL: e.srv.URL})
	inner = e.h
	return e
}

func (e *oidcEnv) addUser(t *testing.T, name, role, email string) store.User {
	t.Helper()
	u, err := e.st.AddUserWithEmail(context.Background(), name, role, e.auth.HashPassword("a-password-of-the-test"), email)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// browser is a cookie jar that does not follow redirects.
func browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// result of a sign-in walked through by a browser.
type signedIn struct {
	status   int    // the callback's answer
	location string // where it sends the browser
	body     string
}

// signIn starts a sign-in landing on next, lets the IdP approve it, and
// follows its redirect back to the callback.
func (e *oidcEnv) signIn(t *testing.T, b *http.Client, next string) signedIn {
	t.Helper()
	res, err := b.PostForm(e.srv.URL+e.pre+"/oidc/test/start", url.Values{"next": {next}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		return signedIn{status: res.StatusCode, body: string(body)}
	}
	res, err = b.Get(res.Header.Get("Location")) // the IdP
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("the IdP answered %d", res.StatusCode)
	}
	res, err = b.Get(res.Header.Get("Location")) // the callback
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	return signedIn{status: res.StatusCode, location: res.Header.Get("Location"), body: string(body)}
}

func (e *oidcEnv) me(t *testing.T, b *http.Client) string {
	t.Helper()
	res, err := b.Get(e.srv.URL + e.pre + "/api/v1/me")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return string(body)
}

// The whole flow: a pre-created user links by verified email, gets an
// ordinary session, and lands on the page asked for; the next sign-in goes
// by issuer and subject.
func TestOIDCSignIn(t *testing.T) {
	for _, prefix := range []string{"/admin", ""} {
		e := newOIDCEnv(t, prefix)
		e.addUser(t, "ann", store.RoleAdmin, "ann@example.test")
		e.idp.Next(oidctest.Claims{Subject: "sub-ann", Email: "Ann@Example.test", EmailVerified: oidctest.Ptr(true)})
		b := browser()
		got := e.signIn(t, b, prefix+"/e/unmatched")
		if got.status != http.StatusSeeOther || got.location != prefix+"/e/unmatched" {
			t.Fatalf("prefix %q: %+v", prefix, got)
		}
		var me struct{ Name, Via string }
		if err := json.Unmarshal([]byte(e.me(t, b)), &me); err != nil || me.Name != "ann" || me.Via != "session" {
			t.Fatalf("prefix %q: me %+v %v", prefix, me, err)
		}
		a := e.idp.Seen()
		if a.RedirectURI != e.srv.URL+prefix+"/oidc/test/callback" || a.Method != "S256" {
			t.Fatalf("prefix %q: the authorization request %+v", prefix, a)
		}
		for _, want := range []string{`msg="oidc identity linked" user=ann provider=test email=ann@example.test`, `msg="user logged in" user=ann via=oidc provider=test`} {
			if !strings.Contains(e.log.String(), want) {
				t.Errorf("prefix %q: no %q in the log", prefix, want)
			}
		}
		// The flow cookie went with the callback; its path was the oidc routes.
		u, _ := url.Parse(e.srv.URL + prefix + "/oidc/test/callback")
		for _, c := range b.Jar.Cookies(u) {
			if c.Name == access.FlowCookieName {
				t.Errorf("prefix %q: the flow cookie stays: %v", prefix, c)
			}
		}
		// Signed in again: by issuer and subject, whatever the email now.
		e.idp.Next(oidctest.Claims{Subject: "sub-ann", Email: "changed@example.test"})
		if got := e.signIn(t, browser(), prefix+"/"); got.status != http.StatusSeeOther {
			t.Fatalf("prefix %q: the second sign-in %+v", prefix, got)
		}
		// next outside the admin is replaced.
		if got := e.signIn(t, browser(), "https://evil.example.test/"); got.location != prefix+"/" {
			t.Fatalf("prefix %q: an outside next: %+v", prefix, got)
		}
	}
}

// Every refusal: its status, its message, one log line with its reason,
// and a failed login counted (the 11th within the window is refused).
func TestOIDCRefusals(t *testing.T) {
	e := newOIDCEnv(t, "/admin")
	ann := e.addUser(t, "ann", store.RoleAdmin, "ann@example.test")
	if _, err := e.st.SignIn(context.Background(), store.SignInInput{Issuer: e.idp.URL, Subject: "ann-first", Provider: "test", Email: "ann@example.test", Verified: true}); err != nil {
		t.Fatal(err)
	}
	_ = ann
	verified := oidctest.Ptr(true)
	for _, c := range []struct {
		reason string
		setup  func()
		status int
		msg    string
	}{
		{"provider_error", func() { e.idp.AuthError("access_denied") }, 401, "TestIdP refused the sign-in (access_denied)."},
		{"invalid_token", func() { e.idp.SetForge(oidctest.Forge{Expired: true}) }, 401, "TestIdP&#39;s answer could not be verified"},
		{"unverified_email", func() {
			e.idp.Next(oidctest.Claims{Subject: "s1", Email: "bob@example.test", EmailVerified: oidctest.Ptr(false)})
		}, 403,
			"TestIdP did not confirm that bob@example.test is yours"},
		{"no_account", func() { e.idp.Next(oidctest.Claims{Subject: "s2", Email: "bob@example.test", EmailVerified: verified}) }, 403,
			"There is no account here for bob@example.test. Ask an admin to add you."},
		{"linked_elsewhere", func() {
			e.idp.Next(oidctest.Claims{Subject: "ann-second", Email: "ann@example.test", EmailVerified: verified})
		}, 403,
			"ann@example.test belongs to an account already signed in with another TestIdP identity"},
	} {
		e.idp.AuthError("")
		e.idp.SetForge(oidctest.Forge{})
		c.setup()
		got := e.signIn(t, browser(), "/admin/")
		if got.status != c.status || !strings.Contains(got.body, c.msg) {
			t.Errorf("%s: %d, %q not in the page", c.reason, got.status, c.msg)
		}
		if strings.Count(e.log.String(), "reason="+c.reason) != 1 {
			t.Errorf("%s: not logged once:\n%s", c.reason, e.log.String())
		}
	}
	e.idp.AuthError("")
	e.idp.SetForge(oidctest.Forge{})
	// expired: a callback without the flow cookie (another browser), and a
	// callback replayed.
	b := browser()
	res, _ := b.PostForm(e.srv.URL+"/admin/oidc/test/start", url.Values{"next": {"/admin/"}})
	res, _ = b.Get(res.Header.Get("Location"))
	callback := res.Header.Get("Location")
	res, _ = browser().Get(callback)
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 401 || !strings.Contains(string(body), "The sign-in took too long or was started in another tab") {
		t.Fatalf("a callback without the flow cookie: %d", res.StatusCode)
	}
	// The browser that started it finishes it; the same callback replayed
	// with the same flow cookie is refused (the flow was taken).
	u, _ := url.Parse(e.srv.URL + "/admin/oidc/test/callback")
	var flow string
	for _, c := range b.Jar.Cookies(u) {
		if c.Name == access.FlowCookieName {
			flow = c.Value
		}
	}
	e.idp.Next(oidctest.Claims{Subject: "ann-first"})
	if res, _ := b.Get(callback); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the callback in its own browser: %d", res.StatusCode)
	}
	replay, _ := http.NewRequest("GET", callback, nil)
	replay.AddCookie(&http.Cookie{Name: access.FlowCookieName, Value: flow})
	if res, _ := browser().Do(replay); flow == "" || res.StatusCode != 401 {
		t.Fatalf("a replayed callback with its flow cookie: %d", res.StatusCode)
	}
	// unreachable: a provider down before its first use (another env, not
	// discovered yet).
	down := newOIDCEnv(t, "/admin")
	down.addUser(t, "ann", store.RoleAdmin, "ann@example.test")
	down.idp.Down(true)
	if got := down.signIn(t, browser(), "/admin/"); got.status != http.StatusBadGateway || !strings.Contains(got.body, "TestIdP cannot be reached right now") ||
		!strings.Contains(down.log.String(), "reason=unreachable") {
		t.Fatalf("unreachable: %+v", got)
	}
	// Password logins work meanwhile.
	if res, _ := browser().PostForm(down.srv.URL+"/admin/login", url.Values{"name": {"ann"}, "password": {"a-password-of-the-test"}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("a password login while the provider is down: %d", res.StatusCode)
	}
	// The sign-in in its own browser cleared this address's failures (a
	// login does); the replay was one. Nine more fill the limit, the next is
	// 429.
	e.idp.AuthError("access_denied")
	for i := range 9 {
		if got := e.signIn(t, browser(), "/admin/"); got.status != 401 {
			t.Fatalf("refusal %d: %d", i+2, got.status)
		}
	}
	if got := e.signIn(t, browser(), "/admin/"); got.status != http.StatusTooManyRequests || !strings.Contains(got.body, "Too many failed logins") {
		t.Fatalf("the 11th refusal: %d", got.status)
	}
}

// With no users, provider sign-in is off: no button, and a start refused.
func TestOIDCLoginOff(t *testing.T) {
	e := newOIDCEnv(t, "/admin", "example.test")
	res, _ := browser().Get(e.srv.URL + "/admin/login")
	body, _ := io.ReadAll(res.Body)
	if strings.Contains(string(body), "Sign in with") {
		t.Fatal("a provider button with login off")
	}
	got := e.signIn(t, browser(), "/admin/")
	if got.status != http.StatusForbidden || !strings.Contains(got.body, "Signing in with a provider starts once the first admin exists.") ||
		!strings.Contains(e.log.String(), "reason=login_off") {
		t.Fatalf("login off: %+v", got)
	}
	if us, _ := e.st.Users(context.Background()); len(us) != 0 {
		t.Fatalf("a user was made: %+v", us)
	}
}

// An allowed domain makes a user with role user and no password.
func TestOIDCAllowedDomain(t *testing.T) {
	e := newOIDCEnv(t, "/admin", "example.test")
	e.addUser(t, "admin", store.RoleAdmin, "")
	e.idp.Next(oidctest.Claims{Subject: "s-carol", Email: "Carol.Lee@example.test", EmailVerified: oidctest.Ptr(true)})
	b := browser()
	if got := e.signIn(t, b, "/admin/"); got.status != http.StatusSeeOther {
		t.Fatalf("%+v", got)
	}
	u, err := e.st.UserByName(context.Background(), "carol.lee")
	if err != nil || u.Role != store.RoleUser || u.HasPassword() || u.Email != "carol.lee@example.test" {
		t.Fatalf("created %+v %v", u, err)
	}
	if !strings.Contains(e.log.String(), `msg="user added" name=carol.lee role=user via=oidc provider=test email=carol.lee@example.test`) {
		t.Fatal("the creation is not logged")
	}
	e.idp.Next(oidctest.Claims{Subject: "s-x", Email: "x@other.test", EmailVerified: oidctest.Ptr(true)})
	if got := e.signIn(t, browser(), "/admin/"); got.status != http.StatusForbidden {
		t.Fatalf("a domain not allowed: %+v", got)
	}
}

// The login page offers one button per provider; opened on another host
// than the admin's origin, the buttons link to the origin's login page, and
// a start posted there is sent to it. A cross-site start is refused.
func TestOIDCOrigins(t *testing.T) {
	e := newOIDCEnv(t, "/admin")
	e.addUser(t, "ann", store.RoleAdmin, "ann@example.test")
	res, _ := browser().Get(e.srv.URL + "/admin/login?next=/admin/e/1")
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), `action="/admin/oidc/test/start"`) || !strings.Contains(string(body), "Sign in with TestIdP") ||
		!strings.Contains(string(body), "or with a password") {
		t.Fatalf("the login page:\n%s", body)
	}
	other := func(method, path string, form url.Values) *http.Response {
		req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(form.Encode()))
		req.Host = "hw.localhost"
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		res, err := browser().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res = other("GET", "/admin/login", nil)
	body, _ = io.ReadAll(res.Body)
	if !strings.Contains(string(body), `href="`+e.srv.URL+`/admin/login?next=`) || strings.Contains(string(body), `action="/admin/oidc/test/start"`) {
		t.Fatalf("the login page on another host:\n%s", body)
	}
	res = other("POST", "/admin/oidc/test/start", url.Values{"next": {"/admin/e/1"}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != e.srv.URL+"/admin/login?moved=1&next=%2Fadmin%2Fe%2F1" {
		t.Fatalf("a start on another host: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	res, _ = browser().Get(e.srv.URL + "/admin/login?moved=1")
	if body, _ := io.ReadAll(res.Body); !strings.Contains(string(body), "Sign in here, at "+e.srv.URL) {
		t.Fatal("no note after a move")
	}
	req, _ := http.NewRequest("POST", e.srv.URL+"/admin/oidc/test/start", strings.NewReader("next=/admin/"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if res, _ := browser().Do(req); res.StatusCode != http.StatusForbidden {
		t.Fatalf("a cross-site start: %d", res.StatusCode)
	}
}

// An https admin URL makes the cookies Secure on its host.
func TestOIDCSecureOrigin(t *testing.T) {
	e := newOIDCEnv(t, "/admin")
	e.addUser(t, "ann", store.RoleAdmin, "ann@example.test")
	e.h.adminOrigin, e.h.adminHost = "https://hooks-admin.example.test", "hooks-admin.example.test"
	r := httptest.NewRequest("POST", "/admin/oidc/test/start", strings.NewReader("next=/admin/"))
	r.Host = "hooks-admin.example.test"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	cs := rec.Result().Cookies()
	if len(cs) != 1 || cs[0].Name != access.FlowCookieName || !cs[0].Secure || cs[0].Path != "/admin/oidc/" {
		t.Fatalf("the flow cookie %+v", cs)
	}
}

// After sign-ins and refusals, nothing the IdP handed out (codes, access
// and ID tokens) nor the client secret is in the log, a page, or the
// database's files.
func TestOIDCKeepsNothingSecret(t *testing.T) {
	e := newOIDCEnv(t, "/admin", "example.test")
	e.addUser(t, "ann", store.RoleAdmin, "ann@example.test")
	var pages strings.Builder
	e.idp.Next(oidctest.Claims{Subject: "sub-ann", Email: "ann@example.test", EmailVerified: oidctest.Ptr(true)})
	b := browser()
	pages.WriteString(e.signIn(t, b, "/admin/").body)
	for _, path := range []string{"/admin/", "/admin/account", "/admin/users", "/admin/api/v1/me", "/admin/api/v1/users"} {
		res, _ := b.Get(e.srv.URL + path)
		body, _ := io.ReadAll(res.Body)
		pages.Write(body)
	}
	for _, f := range []oidctest.Forge{{Expired: true}, {Nonce: "other"}, {Audience: "other"}} {
		e.idp.SetForge(f)
		pages.WriteString(e.signIn(t, browser(), "/admin/").body)
	}
	e.idp.SetForge(oidctest.Forge{})
	e.idp.TokenError("invalid_grant")
	pages.WriteString(e.signIn(t, browser(), "/admin/").body)
	e.st.Close() // flush the WAL into the file
	var db []byte
	for _, f := range []string{e.db, e.db + "-wal"} {
		b, _ := os.ReadFile(f)
		db = append(db, b...)
	}
	if len(db) == 0 {
		t.Fatal("the database files could not be read")
	}
	issued := e.idp.Issued()
	if len(issued) < 6 {
		t.Fatalf("issued %d", len(issued))
	}
	for _, s := range append(issued, oidcSecret) {
		for where, text := range map[string]string{"the log": e.log.String(), "a page": pages.String(), "the database": string(db)} {
			if strings.Contains(text, s) {
				t.Errorf("%s holds something the IdP handed out (or the client secret)", where)
			}
		}
	}
}
