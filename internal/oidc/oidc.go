// Package oidc signs people in to the admin through OpenID Connect
// providers: lazy discovery of each configured provider, the authorization
// code flow with PKCE (S256), state and nonce, kept server-side in memory,
// and the ID token's verification (go-oidc: signature by the provider's
// JWKS, iss, aud, exp; then nonce, azp and sub here). Who the identity is
// in hookwitness is store.SignIn's to decide; the routes are internal/web's.
//
// Nothing a provider hands out is kept or logged: codes, access tokens and
// ID tokens are dropped when a sign-in ends, and no error here quotes one,
// nor the client secret.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Errors of a sign-in, by the reason a refusal gives (internal/web).
var (
	ErrUnreachable  = errors.New("the provider cannot be reached")
	ErrExpired      = errors.New("the sign-in expired or was started elsewhere")
	ErrInvalidToken = errors.New("the provider's answer could not be verified")
)

// ProviderError is an OAuth error the provider sent back (on the callback,
// or from its token endpoint).
type ProviderError struct {
	Code        string // e.g. access_denied
	Description string // the provider's text, for the log only
}

func (e *ProviderError) Error() string { return "the provider refused: " + e.Code }

// Timings.
const (
	FlowLife      = 10 * time.Minute // a sign-in must finish within this
	MaxFlows      = 10_000           // flows in progress; over it the oldest go
	retryAfter    = 30 * time.Second // a failed discovery is tried again after this
	refreshAfter  = 24 * time.Hour   // a discovery document is fetched again after this
	clientTimeout = 10 * time.Second
)

// Config is one provider as configured (internal/config) with its secret.
type Config struct {
	Name, Label    string
	Issuer         string
	ClientID       string
	ClientSecret   string
	RedirectURI    string
	AllowedDomains []string
}

// Provider is a configured provider, discovered when first needed.
type Provider struct {
	Config
	client *http.Client
	now    func() time.Time

	mu      sync.Mutex
	disc    *discovered
	tried   time.Time // the last failed discovery
	lastErr error
}

type discovered struct {
	verifier *gooidc.IDTokenVerifier
	oauth    oauth2.Config
	at       time.Time
}

// NewProvider returns a provider; nothing is fetched yet.
func NewProvider(c Config) *Provider {
	return &Provider{Config: c, now: time.Now, client: &http.Client{
		Timeout: clientTimeout,
		// A provider's endpoints answer where they are; a redirect is not
		// followed (the token endpoint's least of all).
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// SetClock replaces the clock (tests).
func (p *Provider) SetClock(now func() time.Time) { p.now = now }

// SetHTTPClient replaces the client the provider is reached with (tests).
func (p *Provider) SetHTTPClient(c *http.Client) { p.client = c }

// Discover fetches the discovery document now (at start, in the
// background), so the first sign-in does not wait for it.
func (p *Provider) Discover(ctx context.Context) error {
	_, err := p.discovered(ctx)
	return err
}

// AuthOrigin is the origin of the provider's authorization endpoint, where
// a sign-in sends the browser: from the discovery document when fetched,
// else the issuer's (the same at the named providers).
func (p *Provider) AuthOrigin() string {
	p.mu.Lock()
	raw := p.Issuer
	if p.disc != nil {
		raw = p.disc.oauth.Endpoint.AuthURL
	}
	p.mu.Unlock()
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// AllowsDomain reports whether a new identity with this email may become a
// user here.
func (p *Provider) AllowsDomain(email string) bool {
	_, domain, ok := strings.Cut(strings.ToLower(email), "@")
	if !ok {
		return false
	}
	for _, d := range p.AllowedDomains {
		if domain == d {
			return true
		}
	}
	return false
}

// discovered is the provider's discovery, fetched on first use, again after
// refreshAfter (the old one kept while that fails), and after a failure not
// before retryAfter.
func (p *Provider) discovered(ctx context.Context) (*discovered, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if p.disc != nil && now.Sub(p.disc.at) < refreshAfter {
		return p.disc, nil
	}
	if p.lastErr != nil && now.Sub(p.tried) < retryAfter {
		if p.disc != nil {
			return p.disc, nil
		}
		return nil, fmt.Errorf("%w: %v (tried %s ago)", ErrUnreachable, p.lastErr, now.Sub(p.tried).Round(time.Second))
	}
	op, err := gooidc.NewProvider(gooidc.ClientContext(ctx, p.client), p.Issuer)
	if err != nil {
		p.tried, p.lastErr = now, err
		if p.disc != nil {
			return p.disc, nil // keep the endpoints known; keys refetch on their own
		}
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	p.lastErr = nil
	p.disc = &discovered{
		// The JWKS is fetched when a token names a key not seen yet, so a
		// rotated key is picked up; expiry with no leeway, nbf and iat with
		// go-oidc's.
		verifier: op.VerifierContext(gooidc.ClientContext(context.Background(), p.client), &gooidc.Config{ClientID: p.ClientID, Now: p.now}),
		oauth: oauth2.Config{ClientID: p.ClientID, ClientSecret: p.ClientSecret, Endpoint: op.Endpoint(), RedirectURL: p.RedirectURI,
			Scopes: []string{gooidc.ScopeOpenID, "email", "profile"}},
		at: now,
	}
	return p.disc, nil
}

// AuthURL is where a sign-in sends the browser: the authorization endpoint
// with the flow's state, nonce and PKCE challenge (S256).
func (p *Provider) AuthURL(ctx context.Context, f Flow) (string, error) {
	d, err := p.discovered(ctx)
	if err != nil {
		return "", err
	}
	return d.oauth.AuthCodeURL(f.State, gooidc.Nonce(f.Nonce), oauth2.S256ChallengeOption(f.Verifier)), nil
}

// Claims are what a sign-in reads from a verified ID token.
type Claims struct {
	Issuer, Subject string
	Email           string
	Verified        bool // email_verified, or Entra's xms_edov
}

// Exchange trades the callback's code for an ID token, verifies it and
// returns its claims. iss is the callback's iss parameter (RFC 9207; ""
// when the provider sends none).
func (p *Provider) Exchange(ctx context.Context, f Flow, code, iss string) (Claims, error) {
	if iss != "" && iss != p.Issuer {
		return Claims{}, fmt.Errorf("%w: the callback names another issuer", ErrInvalidToken)
	}
	d, err := p.discovered(ctx)
	if err != nil {
		return Claims{}, err
	}
	tok, err := d.oauth.Exchange(gooidc.ClientContext(ctx, p.client), code, oauth2.VerifierOption(f.Verifier))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			// The provider answered: its error code only, never the body.
			c := re.ErrorCode
			if c == "" {
				c = fmt.Sprintf("HTTP %d", re.Response.StatusCode)
			}
			return Claims{}, &ProviderError{Code: c, Description: cut(re.ErrorDescription, 200)}
		}
		return Claims{}, fmt.Errorf("%w: %v", ErrUnreachable, errors.Unwrap(err))
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Claims{}, fmt.Errorf("%w: no id_token in the answer", ErrInvalidToken)
	}
	idt, err := d.verifier.Verify(ctx, raw)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(f.Nonce)) != 1 {
		return Claims{}, fmt.Errorf("%w: the nonce differs", ErrInvalidToken)
	}
	var c struct {
		AZP           string `json:"azp"`
		Email         string `json:"email"`
		EmailVerified any    `json:"email_verified"`
		XMSEdov       any    `json:"xms_edov"`
	}
	if err := idt.Claims(&c); err != nil {
		return Claims{}, fmt.Errorf("%w: claims: %v", ErrInvalidToken, err)
	}
	if c.AZP != "" && c.AZP != p.ClientID {
		return Claims{}, fmt.Errorf("%w: azp names another client", ErrInvalidToken)
	}
	if idt.Subject == "" {
		return Claims{}, fmt.Errorf("%w: no sub", ErrInvalidToken)
	}
	return Claims{Issuer: idt.Issuer, Subject: idt.Subject, Email: c.Email, Verified: truth(c.EmailVerified) || truth(c.XMSEdov)}, nil
}

// truth reads a boolean claim, which some providers send as a string.
func truth(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(b, "true")
	}
	return false
}

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Flow is a sign-in in progress.
type Flow struct {
	Provider string
	State    string
	Nonce    string
	Verifier string // PKCE
	Next     string // the page to land on
	Created  time.Time
}

// Flows keeps the sign-ins in progress in memory, keyed by the SHA-256 of
// their flow id (the browser's cookie): nothing of a flow leaves the server
// but that random id, and a restart forgets them (the user clicks again).
type Flows struct {
	now   func() time.Time
	mu    sync.Mutex
	m     map[[32]byte]Flow
	order [][32]byte // by creation, for eviction
}

// NewFlows returns an empty store; now is the clock (nil: time.Now).
func NewFlows(now func() time.Time) *Flows {
	if now == nil {
		now = time.Now
	}
	return &Flows{now: now, m: map[[32]byte]Flow{}}
}

// Start records a new flow for provider, landing on next, and returns its
// id (for the cookie) and the flow (for AuthURL).
func (fs *Flows) Start(provider, next string) (string, Flow) {
	id := random()
	f := Flow{Provider: provider, State: random(), Nonce: random(), Verifier: oauth2.GenerateVerifier(), Next: next, Created: fs.now()}
	k := sha256.Sum256([]byte(id))
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for len(fs.m) >= MaxFlows && len(fs.order) > 0 {
		delete(fs.m, fs.order[0])
		fs.order = fs.order[1:]
	}
	fs.m[k] = f
	fs.order = append(fs.order, k)
	if len(fs.order) > 2*MaxFlows { // drop keys of flows already taken
		kept := fs.order[:0]
		for _, o := range fs.order {
			if _, ok := fs.m[o]; ok {
				kept = append(kept, o)
			}
		}
		fs.order = kept
	}
	return id, f
}

// Take ends the flow with this id, once: a missing, expired or used flow,
// another provider's, or a state that differs is ErrExpired.
func (fs *Flows) Take(id, provider, state string) (Flow, error) {
	k := sha256.Sum256([]byte(id))
	fs.mu.Lock()
	f, ok := fs.m[k]
	delete(fs.m, k)
	fs.mu.Unlock()
	if !ok || id == "" || fs.now().Sub(f.Created) >= FlowLife || f.Provider != provider ||
		subtle.ConstantTimeCompare([]byte(f.State), []byte(state)) != 1 {
		return Flow{}, ErrExpired
	}
	return f, nil
}

// Len is the number of flows kept (tests).
func (fs *Flows) Len() int { fs.mu.Lock(); defer fs.mu.Unlock(); return len(fs.m) }

func random() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
