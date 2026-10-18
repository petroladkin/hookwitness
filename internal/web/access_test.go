package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/live"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
)

// Test values: every output is searched for them (AC-7).
const (
	adminPW = "admin-password-1234"
	userPW  = "user-password-5678"
	machine = "machine-token-0123456789abcdef0123456789"
)

// lockedBuffer keeps a log written from several goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type accessEnv struct {
	env
	auth *access.Authenticator
	keys *secret.Key
	log  *lockedBuffer
	seen *bytes.Buffer // every answer body, for the secret grep
}

func newAccessEnv(t *testing.T, machineToken string) *accessEnv {
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
	keys, err := secret.Load("", filepath.Join(dir, "hw.db.key"))
	if err != nil {
		t.Fatal(err)
	}
	auth := access.New(st, keys, machineToken)
	logs := &lockedBuffer{}
	lv := live.New()
	h := New(st, lv, slog.New(slog.NewTextHandler(logs, nil)), Options{Prefix: "/admin", MaxBody: 10 << 20, Retention: 1000,
		Hosts: []string{"hooks.example.test", "example.com"}, Keys: keys, Access: auth})
	return &accessEnv{env: env{h, st, lv}, auth: auth, keys: keys, log: logs, seen: &bytes.Buffer{}}
}

func (e *accessEnv) addUser(t *testing.T, name, role, pw string) store.User {
	t.Helper()
	u, err := e.st.AddUser(context.Background(), name, role, e.auth.HashPassword(pw))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// req runs one request with optional credentials: a session cookie value
// and a bearer token.
func (e *accessEnv) req(method, target, cookie, token string, body io.Reader, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, body)
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: access.CookieName, Value: cookie})
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	e.seen.Write(rec.Body.Bytes())
	for _, v := range rec.Result().Header {
		e.seen.WriteString(strings.Join(v, " "))
	}
	return rec
}

func (e *accessEnv) form(target, cookie string, f url.Values) *httptest.ResponseRecorder {
	return e.req("POST", target, cookie, "", strings.NewReader(f.Encode()), "Content-Type", "application/x-www-form-urlencoded")
}

// login logs in and returns the session cookie's value.
func (e *accessEnv) login(t *testing.T, name, pw string) string {
	t.Helper()
	rec := e.form("/admin/login", "", url.Values{"name": {name}, "password": {pw}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login %s: %d %s", name, rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == access.CookieName && c.Value != "" {
			return c.Value
		}
	}
	t.Fatalf("login %s set no cookie", name)
	return ""
}

func (e *accessEnv) token(t *testing.T, u store.User, name string) string {
	t.Helper()
	v, _, err := e.auth.MintFor(context.Background(), u.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestLoginOffIsOpen: with no users the admin behaves as before, and says
// that anyone can use it.
func TestLoginOffIsOpen(t *testing.T) {
	e := newAccessEnv(t, "")
	rec := e.req("GET", "/admin/", "", "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "No login.") {
		t.Fatalf("endpoints page: %d, banner %v", rec.Code, strings.Contains(rec.Body.String(), "No login."))
	}
	if rec := e.req("GET", "/admin/api/v1/endpoints", "", "", nil); rec.Code != 200 {
		t.Fatalf("API with login off: %d", rec.Code)
	}
	if rec := e.req("GET", "/admin/api/v1/me", "", "", nil); !strings.Contains(rec.Body.String(), `"login": false`) {
		t.Fatalf("me: %s", rec.Body.String())
	}
	if rec := e.req("GET", "/admin/users", "", "", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Login is off while there are no users") {
		t.Fatalf("users page with login off: %d", rec.Code)
	}
	if rec := e.req("GET", "/admin/login", "", "", nil); !strings.Contains(rec.Body.String(), "Login is off") {
		t.Fatalf("login page with login off: %s", rec.Body.String())
	}
}

// TestFirstUserTurnsLoginOn: the first user is added on the open users
// page, must be an admin, and from then on the admin needs a login.
func TestFirstUserTurnsLoginOn(t *testing.T) {
	e := newAccessEnv(t, "")
	rec := e.form("/admin/users", "", url.Values{"name": {"Ann"}, "role": {"user"}, "password": {adminPW}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "the first user must be an admin") {
		t.Fatalf("first user as a user: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.form("/admin/users", "", url.Values{"name": {"Ann"}, "role": {"admin"}, "password": {"short"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "at least 12 characters") {
		t.Fatalf("short password: %d", rec.Code)
	}
	rec = e.form("/admin/users", "", url.Values{"name": {"Ann"}, "role": {"admin"}, "password": {adminPW}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/login" {
		t.Fatalf("first admin: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := e.req("GET", "/admin/", "", "", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("endpoints page after the first user: %d", rec.Code)
	}
	c := e.login(t, "ANN", adminPW)
	rec = e.req("GET", "/admin/", c, "", nil)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "No login.") || !strings.Contains(rec.Body.String(), ">ann</a>") {
		t.Fatalf("logged in: %d", rec.Code)
	}
	if !strings.Contains(e.log.String(), "user added") || !strings.Contains(e.log.String(), "user logged in") {
		t.Fatalf("log: %s", e.log.String())
	}
}

// TestRouteMatrix walks every admin route with every kind of credential.
func TestRouteMatrix(t *testing.T) {
	e := newAccessEnv(t, machine)
	admin := e.addUser(t, "ann", store.RoleAdmin, adminPW)
	user := e.addUser(t, "bob", store.RoleUser, userPW)
	adminC, userC := e.login(t, "ann", adminPW), e.login(t, "bob", userPW)
	userT := e.token(t, user, "ci")
	_ = admin
	ep, _ := e.st.CreateEndpoint(context.Background(), "/m")
	id := strings.TrimPrefix(e.h.regPath(ep.ID), "/admin/e/")

	fill := func(p string) string {
		return strings.NewReplacer("{id}", id, "{name}", "nobody", "{reg}", id, "{file...}", "app.css").Replace(p)
	}
	ui := []string{"GET /admin/", "GET /admin/e/{reg}", "GET /admin/e/{reg}/behaviour", "GET /admin/e/{reg}/report",
		"GET /admin/r/1/body", "GET /admin/account", "POST /admin/endpoints", "POST /admin/e/{reg}/clear"}
	for _, pat := range ui {
		method, p, _ := strings.Cut(pat, " ")
		p = fill(p)
		// Nobody: a page load goes to the login page and comes back.
		rec := e.req(method, p, "", "", nil)
		if method == "GET" {
			if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/admin/login?next=") {
				t.Errorf("%s %s without a login: %d %s", method, p, rec.Code, rec.Header().Get("Location"))
			}
		} else if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a login: %d", method, p, rec.Code)
		}
		if method == "GET" {
			if rec := e.req(method, p, "", "", nil, "HX-Request", "true"); rec.Code != http.StatusUnauthorized || rec.Header().Get("HX-Redirect") != "/admin/login" {
				t.Errorf("%s %s from HTMX without a login: %d %q", method, p, rec.Code, rec.Header().Get("HX-Redirect"))
			}
		}
		// A session gets through.
		if rec := e.req(method, p, userC, "", nil); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusSeeOther && strings.Contains(rec.Header().Get("Location"), "/login") {
			t.Errorf("%s %s with a session: %d", method, p, rec.Code)
		}
	}
	// Static files and the login page are open.
	for _, p := range []string{"/admin/login", "/admin/static/app.css"} {
		if rec := e.req("GET", p, "", "", nil); rec.Code != 200 {
			t.Errorf("GET %s without a login: %d", p, rec.Code)
		}
	}
	// The users page: admins only, by session.
	if rec := e.req("GET", "/admin/users", userC, "", nil); rec.Code != http.StatusForbidden {
		t.Errorf("users page for a user: %d", rec.Code)
	}
	if rec := e.req("GET", "/admin/users", adminC, "", nil); rec.Code != 200 {
		t.Errorf("users page for an admin: %d", rec.Code)
	}
	if rec := e.req("GET", "/admin/account", "", userT, nil); rec.Code != http.StatusForbidden {
		t.Errorf("account page with a token: %d", rec.Code)
	}

	for _, pat := range e.h.api.Patterns() {
		method, p, _ := strings.Cut(pat, " ")
		p = fill(p)
		body := func() io.Reader { return strings.NewReader("{}") }
		ct := []string{"Content-Type", "application/json"}
		call := func(cookie, token string) int {
			return e.req(method, p, cookie, token, body(), ct...).Code
		}
		if got := call("", ""); got != http.StatusUnauthorized {
			t.Errorf("%s %s without credentials: %d", method, p, got)
		}
		if got := call("", "hkw_aaaaaaaa_"+strings.Repeat("a", 40)); got != http.StatusUnauthorized {
			t.Errorf("%s %s with a wrong token: %d", method, p, got)
		}
		if got := call(adminC, "wrong"); got != http.StatusUnauthorized {
			t.Errorf("%s %s with a wrong token and a valid cookie: %d", method, p, got)
		}
		sessionOnly := strings.Contains(p, "/me/") || strings.Contains(p, "/users") || strings.Contains(p, "/user/") || strings.Contains(p, "/tokens")
		adminOnly := strings.Contains(p, "/users") || strings.Contains(p, "/user/") || strings.Contains(p, "/api/v1/tokens")
		for _, c := range []struct {
			who           string
			cookie, token string
			want          int // 0: anything but 401 and 403
		}{
			{"admin session", adminC, "", 0},
			{"user session", userC, "", map[bool]int{true: http.StatusForbidden}[adminOnly]},
			{"user token", "", userT, map[bool]int{true: http.StatusForbidden}[sessionOnly]},
			{"machine token", "", machine, map[bool]int{true: http.StatusForbidden}[sessionOnly]},
		} {
			got := call(c.cookie, c.token)
			if c.want == 0 && (got == 401 || got == 403) || c.want != 0 && got != c.want {
				t.Errorf("%s %s as %s: %d", method, p, c.who, got)
			}
		}
	}
}

func TestMCPNeedsAToken(t *testing.T) {
	e := newAccessEnv(t, "")
	u := e.addUser(t, "ann", store.RoleAdmin, adminPW)
	tok := e.token(t, u, "agent")
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	connect := func(hc *http.Client) (*sdk.ClientSession, error) {
		cl := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
		return cl.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: srv.URL + "/admin/mcp", HTTPClient: hc}, nil)
	}
	if _, err := connect(srv.Client()); err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("MCP without a token: %v", err)
	}
	// A browser session is not enough for MCP.
	c := e.login(t, "ann", adminPW)
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`
	if rec := e.req("POST", "/admin/mcp", c, "", strings.NewReader(init), "Content-Type", "application/json", "Accept", "application/json, text/event-stream"); rec.Code != 401 ||
		rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("MCP with a cookie: %d", rec.Code)
	}
	cs, err := connect(withBearer(srv.Client(), tok))
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "create_endpoint", Arguments: map[string]any{"path": "/by/agent"}})
	if err != nil || res.IsError {
		t.Fatalf("tool with a token: %v %v", err, res)
	}
	if !strings.Contains(e.log.String(), "path=/by/agent via=api user=ann") {
		t.Fatalf("the change is not logged with its user: %s", e.log.String())
	}
	// Revoked: the next message is refused.
	pub, _ := access.ParseToken(tok)
	if _, err := e.st.RevokeToken(context.Background(), pub, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.ListTools(context.Background(), nil); err == nil {
		t.Fatal("a revoked token still lists tools")
	}
}

type bearerRT struct {
	base http.RoundTripper
	tok  string
}

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return b.base.RoundTrip(r)
}

func withBearer(c *http.Client, tok string) *http.Client {
	cc := *c
	cc.Transport = bearerRT{c.Transport, tok}
	return &cc
}

// TestTokenSkipsHostCheck: a script with a token may call the server by any
// name; a cookie may not (DNS rebinding).
func TestTokenSkipsHostCheck(t *testing.T) {
	e := newAccessEnv(t, "")
	u := e.addUser(t, "ann", store.RoleAdmin, adminPW)
	tok, c := e.token(t, u, "ci"), e.login(t, "ann", adminPW)
	r := func(cookie, token string) int {
		req := httptest.NewRequest("GET", "/admin/api/v1/endpoints", nil)
		req.Host = "rebound.example.net"
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: access.CookieName, Value: cookie})
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := r("", tok); got != 200 {
		t.Fatalf("token on another host name: %d", got)
	}
	if got := r(c, ""); got != http.StatusMisdirectedRequest {
		t.Fatalf("cookie on another host name: %d", got)
	}
}

func TestLoginAndLimit(t *testing.T) {
	e := newAccessEnv(t, "")
	e.addUser(t, "ann", store.RoleAdmin, adminPW)
	bad := func(name string) *httptest.ResponseRecorder {
		return e.form("/admin/login", "", url.Values{"name": {name}, "password": {"not the password"}})
	}
	if rec := bad("ann"); rec.Code != 401 || !strings.Contains(rec.Body.String(), "Wrong name or password.") {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	if rec := bad("nobody"); rec.Code != 401 || !strings.Contains(rec.Body.String(), "Wrong name or password.") {
		t.Fatalf("unknown name: %d", rec.Code)
	}
	if strings.Contains(e.log.String(), "user=nobody") || !strings.Contains(e.log.String(), "user=ann") {
		t.Fatalf("the log should name only existing users: %s", e.log.String())
	}
	for range access.MaxFailures - 2 {
		bad("ann")
	}
	rec := e.form("/admin/login", "", url.Values{"name": {"ann"}, "password": {adminPW}})
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "Too many failed logins") {
		t.Fatalf("11th attempt, even right: %d", rec.Code)
	}
	// Another address is not limited.
	r := httptest.NewRequest("POST", "/admin/login", strings.NewReader(url.Values{"name": {"ann"}, "password": {adminPW}, "next": {"/admin/users"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "203.0.113.50:1234"
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, r)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/admin/users" {
		t.Fatalf("login from another address: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	var ck *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == access.CookieName {
			ck = c
		}
	}
	if ck == nil || !ck.HttpOnly || ck.Path != "/admin/" || ck.SameSite != http.SameSiteLaxMode || ck.Secure {
		t.Fatalf("cookie %+v", ck)
	}
	// next must stay in the admin.
	for _, next := range []string{"//evil.example/", "https://evil.example/", "/elsewhere", "/admin/login"} {
		r := httptest.NewRequest("POST", "/admin/login", strings.NewReader(url.Values{"name": {"ann"}, "password": {adminPW}, "next": {next}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.RemoteAddr = "203.0.113.51:1234"
		rr := httptest.NewRecorder()
		e.h.ServeHTTP(rr, r)
		if rr.Header().Get("Location") != "/admin/" {
			t.Errorf("next %q led to %q", next, rr.Header().Get("Location"))
		}
	}
}

func TestLogoutAndPasswordChangeEndSessions(t *testing.T) {
	e := newAccessEnv(t, "")
	e.addUser(t, "ann", store.RoleAdmin, adminPW)
	a, b := e.login(t, "ann", adminPW), e.login(t, "ann", adminPW)
	rec := e.form("/admin/account/password", a, url.Values{"current": {adminPW}, "new": {"another-password-99"}, "again": {"another-password-99"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("change password: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.req("GET", "/admin/", a, "", nil); rec.Code != 200 {
		t.Fatalf("the session that changed it: %d", rec.Code)
	}
	if rec := e.req("GET", "/admin/", b, "", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("another session after the change: %d", rec.Code)
	}
	if rec := e.form("/admin/account/password", a, url.Values{"current": {"wrong one at all"}, "new": {"x-password-123456"}, "again": {"x-password-123456"}}); rec.Code != 422 ||
		!strings.Contains(rec.Body.String(), "the current password is wrong") {
		t.Fatalf("wrong current: %d", rec.Code)
	}
	if rec := e.form("/admin/logout", a, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("logout: %d", rec.Code)
	}
	if rec := e.req("GET", "/admin/", a, "", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("after logout: %d", rec.Code)
	}
}

func TestTokenShownOnce(t *testing.T) {
	e := newAccessEnv(t, "")
	e.addUser(t, "ann", store.RoleAdmin, adminPW)
	c := e.login(t, "ann", adminPW)
	rec := e.form("/admin/account/tokens", c, url.Values{"name": {"ci pipeline"}})
	tok := access.TokenRE().FindString(rec.Body.String())
	if rec.Code != 200 || tok == "" || !strings.Contains(rec.Body.String(), "you will not see it again") {
		t.Fatalf("new token page: %d", rec.Code)
	}
	if rec := e.req("GET", "/admin/account", c, "", nil); strings.Contains(rec.Body.String(), tok) || !strings.Contains(rec.Body.String(), "ci pipeline") {
		t.Fatal("the account page shows the token again, or not its name")
	}
	if rec := e.form("/admin/account/tokens", c, url.Values{"name": {"ci pipeline"}}); rec.Code != 422 {
		t.Fatalf("a taken token name: %d", rec.Code)
	}
	if rec := e.req("GET", "/admin/api/v1/endpoints", "", tok, nil); rec.Code != 200 {
		t.Fatalf("API with the new token: %d", rec.Code)
	}
	pub, _ := access.ParseToken(tok)
	if rec := e.form("/admin/account/tokens/"+pub+"/revoke", c, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := e.req("GET", "/admin/api/v1/endpoints", "", tok, nil); rec.Code != 401 {
		t.Fatalf("API with a revoked token: %d", rec.Code)
	}
}

func TestUsersPage(t *testing.T) {
	e := newAccessEnv(t, "")
	e.addUser(t, "ann", store.RoleAdmin, adminPW)
	bob := e.addUser(t, "bob", store.RoleUser, userPW)
	a, b := e.login(t, "ann", adminPW), e.login(t, "bob", userPW)
	bt := e.token(t, bob, "ci")
	if rec := e.form("/admin/users", b, url.Values{"name": {"eve"}, "role": {"admin"}, "password": {userPW}}); rec.Code != http.StatusForbidden {
		t.Fatalf("a user adds a user: %d", rec.Code)
	}
	rec := e.req("GET", "/admin/users", a, "", nil)
	if !strings.Contains(rec.Body.String(), "the last admin") || !strings.Contains(rec.Body.String(), "Make admin") {
		t.Fatal("users page: the last admin is not marked")
	}
	if rec := e.form("/admin/user/ann/remove", a, nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "last admin") {
		t.Fatalf("removing the last admin: %d", rec.Code)
	}
	if rec := e.form("/admin/user/ann/role", a, url.Values{"role": {"user"}}); rec.Code != http.StatusConflict {
		t.Fatalf("demoting the last admin: %d", rec.Code)
	}
	// A reset by an admin ends that user's sessions; their tokens keep working.
	if rec := e.form("/admin/user/bob/password", a, url.Values{"password": {"bob-new-password-1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("reset: %d", rec.Code)
	}
	if rec := e.req("GET", "/admin/", b, "", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("bob's session after a reset: %d", rec.Code)
	}
	if rec := e.req("GET", "/admin/api/v1/endpoints", "", bt, nil); rec.Code != 200 {
		t.Fatalf("bob's token after a reset: %d", rec.Code)
	}
	if rec := e.form("/admin/user/bob/role", a, url.Values{"role": {"admin"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("promote: %d", rec.Code)
	}
	if rec := e.req("GET", "/admin/api/v1/users", "", bt, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("admin's token on user management: %d", rec.Code)
	}
	if rec := e.form("/admin/user/bob/remove", a, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("remove: %d", rec.Code)
	}
	if rec := e.req("GET", "/admin/api/v1/endpoints", "", bt, nil); rec.Code != 401 {
		t.Fatalf("token of a removed user: %d", rec.Code)
	}
	if !strings.Contains(e.log.String(), "user removed") || !strings.Contains(e.log.String(), "name=bob user=ann") {
		t.Fatalf("log: %s", e.log.String())
	}
}

// TestLiveFeedEndsWithTheSession: an open register sees its login end and
// is told to reload.
func TestLiveFeedEndsWithTheSession(t *testing.T) {
	old := sessionCheck
	sessionCheck = 50 * time.Millisecond
	t.Cleanup(func() { sessionCheck = old })
	e := newAccessEnv(t, "")
	e.addUser(t, "ann", store.RoleAdmin, adminPW)
	c := e.login(t, "ann", adminPW)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/live")
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+e.h.regPath(ep.ID)+"/events", nil)
	req.AddCookie(&http.Cookie{Name: access.CookieName, Value: c})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("feed: %d", resp.StatusCode)
	}
	se, _ := e.st.SessionByHash(context.Background(), access.HashSession(c))
	e.st.EndSession(context.Background(), se.ID)
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(resp.Body); done <- string(b) }()
	select {
	case got := <-done:
		if !strings.Contains(got, "event: login") {
			t.Fatalf("feed ended without the login event: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the feed did not end with its session")
	}
}

// TestNoSecretInOutputs runs the flows above and searches every answer and
// the log for the passwords, tokens, session ids and stored records (AC-7).
func TestNoSecretInOutputs(t *testing.T) {
	e := newAccessEnv(t, machine)
	e.form("/admin/users", "", url.Values{"name": {"ann"}, "role": {"admin"}, "password": {adminPW}})
	a := e.login(t, "ann", adminPW)
	e.form("/admin/users", a, url.Values{"name": {"bob"}, "role": {"user"}, "password": {userPW}})
	b := e.login(t, "bob", userPW)
	e.form("/admin/login", "", url.Values{"name": {"bob"}, "password": {adminPW}})
	rec := e.form("/admin/account/tokens", b, url.Values{"name": {"x"}})
	tok := access.TokenRE().FindString(rec.Body.String())
	shown := e.seen.Len() // the one page that shows the new token
	for _, p := range []string{"/admin/", "/admin/account", "/admin/users", "/admin/api/v1/me", "/admin/api/v1/me/tokens"} {
		e.req("GET", p, b, "", nil)
		e.req("GET", p, a, "", nil)
	}
	e.req("GET", "/admin/api/v1/tokens", a, "", nil)
	e.req("GET", "/admin/api/v1/users", a, "", nil)
	e.req("GET", "/admin/api/v1/endpoints", "", machine, nil)
	e.req("GET", "/admin/api/v1/endpoints", "", tok, nil)
	e.req("PUT", "/admin/api/v1/me/password", b, "", strings.NewReader(`{"current":"`+userPW+`","new":"`+userPW+`-2"}`), "Content-Type", "application/json")
	us, _ := e.st.Users(context.Background())
	secrets := []string{adminPW, userPW, machine, a, b}
	for _, u := range us {
		secrets = append(secrets, u.Password)
	}
	after := e.seen.String()[shown:]
	for _, s := range secrets {
		if strings.Contains(e.log.String(), s) {
			t.Errorf("the log holds a secret value %q…", s[:6])
		}
		if strings.Contains(after, s) {
			t.Errorf("an answer holds a secret value %q…", s[:6])
		}
	}
	if strings.Contains(after, tok) || strings.Contains(e.log.String(), tok) {
		t.Error("the token appears after the page that made it")
	}
}
