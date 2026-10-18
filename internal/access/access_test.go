package access

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
)

func TestCheckName(t *testing.T) {
	for in, want := range map[string]string{"Alice": "alice", " bob.ops_1-x ": "bob.ops_1-x", strings.Repeat("a", 64): strings.Repeat("a", 64)} {
		if got, err := CheckName(in); err != nil || got != want {
			t.Errorf("CheckName(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", " ", "a b", "ann@x", "é", strings.Repeat("a", 65), "a/b"} {
		if _, err := CheckName(in); err == nil {
			t.Errorf("CheckName(%q) accepted", in)
		} else if e, ok := IsInvalid(err); !ok || e.Field != "name" {
			t.Errorf("CheckName(%q): %v", in, err)
		}
	}
}

func TestCheckPassword(t *testing.T) {
	if err := CheckPassword("eleven char"); err == nil {
		t.Error("11 characters accepted")
	}
	if err := CheckPassword("twelve chars"); err != nil {
		t.Errorf("12 characters: %v", err)
	}
	if err := CheckPassword("пароль-довгий"); err != nil { // 13 characters, 25 bytes
		t.Errorf("non-ASCII: %v", err)
	}
	if err := CheckPassword(strings.Repeat("x", MaxPassword+1)); err == nil {
		t.Error("over the byte limit accepted")
	}
}

func TestTokens(t *testing.T) {
	re := regexp.MustCompile(`^` + TokenPattern + `$`)
	a, ida := MintToken()
	b, _ := MintToken()
	if a == b || !re.MatchString(a) || !re.MatchString(b) {
		t.Fatalf("minted %q %q", a, b)
	}
	if id, ok := ParseToken(a); !ok || id != ida || !strings.HasPrefix(a, "hkw_"+ida+"_") {
		t.Fatalf("ParseToken(%q) = %q %v", a, id, ok)
	}
	for _, bad := range []string{"", "hkw_", a + "a", a[:len(a)-1], strings.ToUpper(a), "hkx" + a[3:], " " + a, strings.Replace(a, "_", "-", 1),
		"hkw_abcdefg1_" + strings.Repeat("a", 40)} {
		if _, ok := ParseToken(bad); ok {
			t.Errorf("ParseToken(%q) accepted", bad)
		}
	}
	if got := Shown(ida); got != "hkw_"+ida+"_…" {
		t.Errorf("Shown = %q", got)
	}
}

func TestBearerHeader(t *testing.T) {
	for h, want := range map[string]struct {
		tok     string
		present bool
	}{
		"":                {"", false},
		"Bearer abc":      {"abc", true},
		"bearer  abc ":    {"abc", true},
		"Bearer":          {"", true},
		"Basic dXNlcjpw":  {"", false}, // a proxy's login in front of the admin
		"Token something": {"", false},
	} {
		tok, present := bearer(h)
		if tok != want.tok || present != want.present {
			t.Errorf("bearer(%q) = %q %v", h, tok, present)
		}
	}
}

func TestIdentityContext(t *testing.T) {
	ctx := context.Background()
	if From(ctx) != nil || Who(ctx) != nil {
		t.Fatal("an empty context has an identity")
	}
	id := &Identity{Name: "ann", Role: store.RoleAdmin, Via: ViaSession}
	ctx = With(ctx, id)
	if From(ctx) != id || !From(ctx).Admin() {
		t.Fatal("identity lost")
	}
	if w := Who(ctx); len(w) != 2 || w[0] != "user" || w[1] != "ann" {
		t.Fatalf("Who = %v", w)
	}
	if (&Identity{Role: store.RoleUser}).Admin() || (*Identity)(nil).Admin() {
		t.Fatal("a user is an admin")
	}
}

func TestCookie(t *testing.T) {
	c := Cookie("abc", "/admin", false)
	if c.Name != CookieName || c.Path != "/admin/" || !c.HttpOnly || c.Secure || c.SameSite != http.SameSiteLaxMode || c.MaxAge != 0 {
		t.Fatalf("cookie %+v", c)
	}
	if c := Cookie("abc", "", true); c.Path != "/" || !c.Secure {
		t.Fatalf("root cookie %+v", c)
	}
	if c := Cookie("", "/admin", false); c.MaxAge >= 0 {
		t.Fatalf("clearing cookie %+v", c)
	}
}

func TestExpired(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		created, seen, now time.Time
		want               bool
	}{
		{t0, t0, t0.Add(23 * time.Hour), false},
		{t0, t0, t0.Add(24*time.Hour + time.Second), true},                           // idle
		{t0, t0.Add(6 * 24 * time.Hour), t0.Add(6*24*time.Hour + time.Hour), false},  // active
		{t0, t0.Add(7 * 24 * time.Hour), t0.Add(7*24*time.Hour + time.Second), true}, // absolute
	} {
		if got := Expired(c.created, c.seen, c.now); got != c.want {
			t.Errorf("Expired(%v, %v, %v) = %v", c.created, c.seen, c.now, got)
		}
	}
	id, hash := NewSessionID()
	if len(id) != 43 || !validSessionID(id) || string(HashSession(id)) != string(hash) {
		t.Fatalf("session id %q", id)
	}
	if validSessionID("short") || validSessionID(strings.Repeat("*", 43)) {
		t.Fatal("junk accepted as a session id")
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestLimiter(t *testing.T) {
	c := &clock{time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	l := NewLimiter(c.now)
	for i := range MaxFailures {
		if ok, _ := l.Allow("203.0.113.1"); !ok {
			t.Fatalf("attempt %d refused", i+1)
		}
		l.Fail("203.0.113.1")
		c.t = c.t.Add(time.Minute)
	}
	ok, wait := l.Allow("203.0.113.1")
	if ok {
		t.Fatal("the 11th attempt allowed")
	}
	// The first failure was 10 minutes ago: it leaves the window in 5.
	if wait != 5*time.Minute {
		t.Fatalf("wait %v", wait)
	}
	if ok, _ := l.Allow("203.0.113.2"); !ok {
		t.Fatal("another address limited")
	}
	c.t = c.t.Add(5 * time.Minute)
	if ok, _ := l.Allow("203.0.113.1"); !ok {
		t.Fatal("still limited after the window slid")
	}
	l.Fail("203.0.113.1")
	if ok, _ := l.Allow("203.0.113.1"); ok {
		t.Fatal("a new failure inside the window should limit again")
	}
	l.Success("203.0.113.1")
	if ok, _ := l.Allow("203.0.113.1"); !ok {
		t.Fatal("success did not clear the count")
	}
}

func TestLimiterCap(t *testing.T) {
	l := NewLimiter(nil)
	for i := range maxTracked + 5 {
		l.Fail("a" + strings.Repeat("x", i%7) + string(rune('0'+i%10)) + time.Duration(i).String())
	}
	if len(l.fails) > maxTracked {
		t.Fatalf("%d addresses tracked", len(l.fails))
	}
}

func TestClientAddr(t *testing.T) {
	ps, err := ParseProxies("198.51.100.0/24, 192.0.2.5")
	if err != nil {
		t.Fatal(err)
	}
	req := func(peer string, xff ...string) *http.Request {
		r := httptest.NewRequest("POST", "/admin/login", nil)
		r.RemoteAddr = peer
		for _, v := range xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		return r
	}
	for _, c := range []struct {
		r    *http.Request
		ps   Proxies
		want string
	}{
		{req("203.0.113.9:5555", "1.1.1.1"), ps, "203.0.113.9"},                       // untrusted peer: header ignored
		{req("198.51.100.3:5555", "1.1.1.1"), nil, "198.51.100.3"},                    // no proxies configured
		{req("198.51.100.3:5555", "6.6.6.6, 1.1.1.1"), ps, "1.1.1.1"},                 // rightmost untrusted
		{req("198.51.100.3:5555", "1.1.1.1, 192.0.2.5"), ps, "1.1.1.1"},               // through two proxies
		{req("198.51.100.3:5555", "6.6.6.6", "1.1.1.1, 198.51.100.9"), ps, "1.1.1.1"}, // several header lines
		{req("198.51.100.3:5555", "garbage"), ps, "198.51.100.3"},                     // damaged header
		{req("198.51.100.3:5555"), ps, "198.51.100.3"},                                // no header
		{req("[::ffff:198.51.100.3]:5555", "2001:db8::1"), ps, "2001:db8::1"},         // mapped peer
	} {
		if got := ClientAddr(c.r, c.ps); got != c.want {
			t.Errorf("ClientAddr(%s, %v) = %s, want %s", c.r.RemoteAddr, c.r.Header.Values("X-Forwarded-For"), got, c.want)
		}
	}
	if _, err := ParseProxies("198.51.100.0/24, nope"); err == nil {
		t.Error("garbage accepted")
	}
	if ps, err := ParseProxies(""); err != nil || len(ps) != 0 {
		t.Errorf("empty: %v %v", ps, err)
	}
}

func newAuth(t *testing.T, machine string) (*Authenticator, *store.Store, *secret.Key) {
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
	return New(st, keys, machine), st, keys
}

func TestAuthenticator(t *testing.T) {
	ctx := context.Background()
	a, st, _ := newAuth(t, "")
	if on, _ := a.LoginOn(ctx); on {
		t.Fatal("login on without users")
	}
	if on, _ := a.APILoginOn(ctx); on {
		t.Fatal("API login on without users or a machine token")
	}
	u, err := st.AddUser(ctx, "ann", store.RoleAdmin, a.HashPassword("correct horse battery"))
	if err != nil {
		t.Fatal(err)
	}
	if on, _ := a.LoginOn(ctx); !on {
		t.Fatal("login off with a user")
	}
	if _, err := a.Login(ctx, "ann", "wrong password!!"); !errors.Is(err, ErrWrongLogin) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := a.Login(ctx, "nobody", "correct horse battery"); !errors.Is(err, ErrWrongLogin) {
		t.Fatalf("unknown user: %v", err)
	}
	got, err := a.Login(ctx, " ANN ", "correct horse battery")
	if err != nil || got.ID != u.ID {
		t.Fatalf("login: %+v, %v", got, err)
	}

	c := &clock{time.Now()}
	a.SetClock(c.now)
	sid, rowID, err := a.StartSession(ctx, got)
	if err != nil {
		t.Fatal(err)
	}
	id, err := a.Session(ctx, sid)
	if err != nil || id.Name != "ann" || id.Via != ViaSession || id.SessionID != rowID || !id.Admin() {
		t.Fatalf("session: %+v, %v", id, err)
	}
	if _, err := a.Session(ctx, sid[:42]+"A"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("another id: %v", err)
	}
	// Activity keeps it; a day without ends it.
	c.t = c.t.Add(23 * time.Hour)
	if _, err := a.Session(ctx, sid); err != nil {
		t.Fatalf("after 23 h: %v", err)
	}
	c.t = c.t.Add(24*time.Hour + time.Minute)
	if _, err := a.Session(ctx, sid); !errors.Is(err, ErrNoSession) {
		t.Fatalf("after a day idle: %v", err)
	}
	if _, err := st.SessionByHash(ctx, HashSession(sid)); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("an ended session was kept")
	}

	// Tokens act with the owner's current role and stop when revoked.
	tok, rec, err := a.MintFor(ctx, u.ID, "ci")
	if err != nil {
		t.Fatal(err)
	}
	ti, present, err := a.Bearer(ctx, "Bearer "+tok)
	if err != nil || !present || ti.Via != ViaToken || ti.TokenID != rec.PublicID || !ti.Admin() {
		t.Fatalf("bearer: %+v %v %v", ti, present, err)
	}
	if _, present, _ := a.Bearer(ctx, "Basic eDp5"); present {
		t.Fatal("a Basic header taken for a token")
	}
	for _, bad := range []string{"Bearer ", "Bearer " + tok + "x", "Bearer " + tok[:len(tok)-1] + "a"} {
		if strings.HasSuffix(bad, "a") && strings.HasSuffix(tok, "a") {
			continue
		}
		if _, _, err := a.Bearer(ctx, bad); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("Bearer(%q): %v", bad, err)
		}
	}
	if _, err := st.AddUser(ctx, "bob", store.RoleAdmin, "r"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetRole(ctx, "ann", store.RoleUser); err != nil {
		t.Fatal(err)
	}
	if ti, _, _ := a.Bearer(ctx, "Bearer "+tok); ti == nil || ti.Admin() {
		t.Fatalf("token after demotion: %+v", ti)
	}
	if _, err := st.RevokeToken(ctx, rec.PublicID, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Bearer(ctx, "Bearer "+tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked token: %v", err)
	}
}

func TestMachineToken(t *testing.T) {
	ctx := context.Background()
	const m = "hkw_machine_0123456789abcdef0123456789abcdef"
	a, _, _ := newAuth(t, m)
	if on, _ := a.APILoginOn(ctx); !on {
		t.Fatal("a machine token leaves the API open")
	}
	if on, _ := a.LoginOn(ctx); on {
		t.Fatal("a machine token turned the UI's login on")
	}
	id, _, err := a.Bearer(ctx, "Bearer "+m)
	if err != nil || id.Via != ViaMachine || id.Name != MachineName || id.Role != store.RoleUser {
		t.Fatalf("machine: %+v, %v", id, err)
	}
	if _, _, err := a.Bearer(ctx, "Bearer "+m+"x"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("near miss: %v", err)
	}

	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode)
		return p
	}
	if tok, warn, err := ReadMachineToken(write("ok", m+"\n", 0o600)); err != nil || tok != m || warn != "" {
		t.Fatalf("read: %q %q %v", tok, warn, err)
	}
	if _, warn, err := ReadMachineToken(write("open", m, 0o644)); err != nil || warn == "" {
		t.Fatalf("world-readable: %q %v", warn, err)
	}
	for name, body := range map[string]string{"short": "too short\n", "two": m + "\n" + m + "\n", "space": " " + m} {
		if _, _, err := ReadMachineToken(write(name, body, 0o600)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, _, err := ReadMachineToken(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file accepted")
	}
}
