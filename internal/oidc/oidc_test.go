package oidc

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/oidc/oidctest"
)

const (
	clientID = "hw-client"
	secret   = "hw-client-secret-for-tests"
	redirect = "https://hooks.example.test/admin/oidc/test/callback"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }
func newClock() *clock               { return &clock{t: time.Now()} }
func provider(idp *oidctest.Provider) *Provider {
	return NewProvider(Config{Name: "test", Label: "Test", Issuer: idp.URL, ClientID: clientID, ClientSecret: secret, RedirectURI: redirect})
}

// signIn walks a browser through the provider: the auth URL, the provider's
// redirect back, and the exchange; it returns the claims or the error.
func signIn(t *testing.T, p *Provider, fs *Flows) (Claims, error) {
	t.Helper()
	id, f := fs.Start("test", "/admin/")
	u, err := p.AuthURL(context.Background(), f)
	if err != nil {
		return Claims{}, err
	}
	res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(u)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	back, _ := url.Parse(res.Header.Get("Location"))
	q := back.Query()
	got, err := fs.Take(id, "test", q.Get("state"))
	if err != nil {
		return Claims{}, err
	}
	if e := q.Get("error"); e != "" {
		return Claims{}, &ProviderError{Code: e}
	}
	return p.Exchange(context.Background(), got, q.Get("code"), q.Get("iss"))
}

func TestSignInAndClaims(t *testing.T) {
	idp := oidctest.New(clientID, secret)
	defer idp.Close()
	p, fs := provider(idp), NewFlows(nil)
	idp.Next(oidctest.Claims{Subject: "u-1", Email: "Ann@Example.test", EmailVerified: oidctest.Ptr(true)})
	c, err := signIn(t, p, fs)
	if err != nil || c.Subject != "u-1" || c.Issuer != idp.URL || c.Email != "Ann@Example.test" || !c.Verified {
		t.Fatalf("%+v %v", c, err)
	}
	a := idp.Seen()
	if a.Method != "S256" || a.Challenge == "" || a.Nonce == "" || a.State == "" || a.RedirectURI != redirect || a.Scope != "openid email profile" {
		t.Fatalf("the authorization request: %+v", a)
	}
	for _, v := range []struct {
		name   string
		claims oidctest.Claims
		want   bool
	}{
		{"email_verified false", oidctest.Claims{Subject: "u", Email: "a@x.test", EmailVerified: oidctest.Ptr(false)}, false},
		{"no email_verified", oidctest.Claims{Subject: "u", Email: "a@x.test"}, false},
		{"Entra's xms_edov", oidctest.Claims{Subject: "u", Email: "a@x.test", XMSEdov: oidctest.Ptr(true)}, true},
		{"several audiences and our azp", oidctest.Claims{Subject: "u", Email: "a@x.test", EmailVerified: oidctest.Ptr(true), Audiences: []string{"other"}, AZP: clientID}, true},
	} {
		idp.Next(v.claims)
		c, err := signIn(t, p, fs)
		if err != nil || c.Verified != v.want {
			t.Errorf("%s: %+v %v", v.name, c, err)
		}
	}
	idp.Next(oidctest.Claims{Subject: "u", Audiences: []string{"other"}, AZP: "other"})
	if _, err := signIn(t, p, fs); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("azp of another client: %v", err)
	}
	idp.Next(oidctest.Claims{})
	if _, err := signIn(t, p, fs); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("no sub: %v", err)
	}
}

// Every forged token is refused as invalid, and no error quotes a code, a
// token or the client secret.
func TestForgedTokens(t *testing.T) {
	idp := oidctest.New(clientID, secret)
	defer idp.Close()
	p, fs := provider(idp), NewFlows(nil)
	idp.Next(oidctest.Claims{Subject: "u-1", Email: "a@x.test", EmailVerified: oidctest.Ptr(true)})
	var errs []string
	for name, f := range map[string]oidctest.Forge{
		"another issuer":            {Issuer: "https://evil.example.test"},
		"another audience":          {Audience: "someone-else"},
		"expired":                   {Expired: true},
		"not yet valid":             {NotYet: true},
		"another nonce":             {Nonce: "replayed"},
		"alg none":                  {AlgNone: true},
		"HS256 with the public key": {HS256: true},
		"an unknown key":            {UnknownKid: true},
	} {
		idp.SetForge(f)
		_, err := signIn(t, p, fs)
		if !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: %v, want an invalid token", name, err)
			continue
		}
		errs = append(errs, err.Error())
	}
	idp.SetForge(oidctest.Forge{})
	// A rotated key is fetched by its kid.
	idp.Rotate()
	if _, err := signIn(t, p, fs); err != nil {
		t.Fatalf("after a rotation: %v", err)
	}
	// The callback's iss of another issuer (a mix-up between providers).
	idp.IssParam("https://other-idp.example.test")
	if _, err := signIn(t, p, fs); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("an iss parameter of another issuer: %v", err)
	}
	idp.IssParam(idp.URL)
	if _, err := signIn(t, p, fs); err != nil {
		t.Fatalf("the iss parameter of the issuer: %v", err)
	}
	idp.IssParam("")
	// The provider refuses at the authorization or the token endpoint.
	idp.AuthError("access_denied")
	if _, err := signIn(t, p, fs); !errors.As(err, new(*ProviderError)) {
		t.Fatalf("access denied: %v", err)
	}
	idp.AuthError("")
	idp.TokenError("invalid_grant")
	_, err := signIn(t, p, fs)
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Code != "invalid_grant" {
		t.Fatalf("a token endpoint error: %v", err)
	}
	errs = append(errs, err.Error())
	idp.TokenError("")
	for _, e := range errs {
		for _, s := range append(idp.Issued(), secret) {
			if strings.Contains(e, s) {
				t.Fatalf("an error quotes something secret: %s", e)
			}
		}
	}
}

func TestDiscovery(t *testing.T) {
	idp := oidctest.New(clientID, secret)
	defer idp.Close()
	c := newClock()
	p := provider(idp)
	p.SetClock(c.now)
	idp.Down(true)
	if err := p.Discover(context.Background()); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("down at start: %v", err)
	}
	idp.Down(false)
	if err := p.Discover(context.Background()); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("retried within 30 s: %v", err)
	}
	c.add(31 * time.Second)
	if err := p.Discover(context.Background()); err != nil {
		t.Fatalf("up again, retried after 30 s: %v", err)
	}
	// Known, the provider is used while down; after 24 h a failed refresh
	// keeps the endpoints known.
	idp.Down(true)
	c.add(25 * time.Hour)
	if err := p.Discover(context.Background()); err != nil {
		t.Fatalf("a failed refresh: %v", err)
	}
	idp.Down(false)
	// A discovery document naming another issuer is refused.
	wrong := NewProvider(Config{Name: "x", Issuer: idp.URL + "/", ClientID: clientID, ClientSecret: secret, RedirectURI: redirect})
	if err := wrong.Discover(context.Background()); !errors.Is(err, ErrUnreachable) || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("issuer mismatch: %v", err)
	}
	// The token endpoint does not answer in time.
	p2 := provider(idp)
	p2.SetHTTPClient(&http.Client{Timeout: 200 * time.Millisecond})
	idp.TokenDelay(time.Second)
	idp.Next(oidctest.Claims{Subject: "u"})
	if _, err := signIn(t, p2, NewFlows(nil)); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("a slow token endpoint: %v", err)
	}
}

func TestFlows(t *testing.T) {
	c := newClock()
	fs := NewFlows(c.now)
	id, f := fs.Start("google", "/admin/e/1")
	if f.Provider != "google" || f.Next != "/admin/e/1" || len(f.State) < 40 || len(f.Nonce) < 40 || len(f.Verifier) < 43 || len(id) < 40 {
		t.Fatalf("flow %+v id %q", f, id)
	}
	if _, err := fs.Take(id, "google", f.State); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Take(id, "google", f.State); !errors.Is(err, ErrExpired) {
		t.Fatalf("used twice: %v", err)
	}
	for name, take := range map[string]func(id string, f Flow) error{
		"another provider": func(id string, f Flow) error { _, err := fs.Take(id, "entra", f.State); return err },
		"another state":    func(id string, f Flow) error { _, err := fs.Take(id, "google", f.State+"x"); return err },
		"no flow id":       func(id string, f Flow) error { _, err := fs.Take("", "google", f.State); return err },
		"after 10 minutes": func(id string, f Flow) error { c.add(FlowLife); _, err := fs.Take(id, "google", f.State); return err },
	} {
		id, f := fs.Start("google", "/admin/")
		if err := take(id, f); !errors.Is(err, ErrExpired) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The cap: the oldest flows go.
	first, ff := fs.Start("google", "/")
	for range MaxFlows {
		fs.Start("google", "/")
	}
	if fs.Len() > MaxFlows {
		t.Fatalf("%d flows kept", fs.Len())
	}
	if _, err := fs.Take(first, "google", ff.State); !errors.Is(err, ErrExpired) {
		t.Fatalf("the oldest flow outlived the cap: %v", err)
	}
}

func TestAllowsDomain(t *testing.T) {
	p := NewProvider(Config{AllowedDomains: []string{"example.test"}})
	for email, want := range map[string]bool{"ann@example.test": true, "Ann@EXAMPLE.test": true, "ann@sub.example.test": false, "ann@example.test.evil": false, "no-at": false} {
		if got := p.AllowsDomain(email); got != want {
			t.Errorf("%s: %v", email, got)
		}
	}
}
