// Package oidctest is an OpenID Connect provider for tests: discovery, a
// JWKS, an authorization endpoint that approves at once, and a token
// endpoint that checks the client secret, the PKCE verifier and the
// redirect URI, then returns an RS256 ID token for the identity the test
// set. Knobs forge the tokens a verifier must refuse, rotate the key and
// take the provider down. Test-only: never imported by the server.
package oidctest

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Claims is the identity the provider returns next.
type Claims struct {
	Subject       string
	Email         string
	EmailVerified *bool // nil: no email_verified claim
	XMSEdov       *bool // Entra's optional claim; nil: absent
	Audiences     []string
	AZP           string // "" : no azp claim
}

// Forge makes the next ID tokens wrong in one way each.
type Forge struct {
	Issuer     string // another iss
	Audience   string // another aud (instead of the client id)
	Expired    bool
	NotYet     bool   // nbf and iat an hour ahead
	Nonce      string // another nonce
	AlgNone    bool   // alg: none, no signature
	HS256      bool   // HMAC-SHA256 keyed with the public key's PEM (alg confusion)
	UnknownKid bool   // signed by a key the JWKS does not have
}

// Authorize is what the last authorization request asked for.
type Authorize struct {
	ClientID, RedirectURI, Scope, State, Nonce, Challenge, Method, ResponseType string
}

type code struct {
	redirect, nonce, challenge string
	used                       bool
}

type key struct {
	kid string
	key *rsa.PrivateKey
}

// Provider is a running test provider. Its URL is the issuer.
type Provider struct {
	URL          string
	ClientID     string
	ClientSecret string

	srv *http.Server
	ln  net.Listener

	mu         sync.Mutex
	keys       []key // the JWKS; the last signs
	next       Claims
	forge      Forge
	codes      map[string]*code
	down       bool
	tokenDelay time.Duration
	tokenError string // the token endpoint answers this OAuth error
	authError  string // the authorization endpoint redirects with this error
	issParam   string // sent as iss on the redirect back; "" none
	seen       Authorize
	issued     []string // codes, access tokens and ID tokens handed out
}

// New starts a provider on a loopback port.
func New(clientID, clientSecret string) *Provider {
	p, err := Listen("127.0.0.1:0", clientID, clientSecret)
	if err != nil {
		panic(err)
	}
	return p
}

// Listen starts a provider on addr (scripts/oidcmock).
func Listen(addr, clientID, clientSecret string) (*Provider, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	p := &Provider{ClientID: clientID, ClientSecret: clientSecret, ln: ln, codes: map[string]*code{},
		URL: "http://" + ln.Addr().String()}
	p.keys = []key{newKey()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /jwks", p.jwks)
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	mux.HandleFunc("POST /_test/next", p.setNext)
	p.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go p.srv.Serve(ln)
	return p, nil
}

// Close stops the provider.
func (p *Provider) Close() { p.srv.Close() }

func newKey() key {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	b := make([]byte, 8)
	rand.Read(b)
	return key{kid: base64.RawURLEncoding.EncodeToString(b), key: k}
}

// Next sets the identity the next tokens carry.
func (p *Provider) Next(c Claims) { p.mu.Lock(); p.next = c; p.mu.Unlock() }

// SetForge makes the next tokens wrong (a zero Forge: right again).
func (p *Provider) SetForge(f Forge) { p.mu.Lock(); p.forge = f; p.mu.Unlock() }

// Rotate adds a new signing key; the old one stays in the JWKS.
func (p *Provider) Rotate() { p.mu.Lock(); p.keys = append(p.keys, newKey()); p.mu.Unlock() }

// Kid is the current signing key's id.
func (p *Provider) Kid() string { p.mu.Lock(); defer p.mu.Unlock(); return p.keys[len(p.keys)-1].kid }

// Down makes discovery, the JWKS and the token endpoint answer 503.
func (p *Provider) Down(down bool) { p.mu.Lock(); p.down = down; p.mu.Unlock() }

// TokenDelay holds the token endpoint's answer.
func (p *Provider) TokenDelay(d time.Duration) { p.mu.Lock(); p.tokenDelay = d; p.mu.Unlock() }

// TokenError makes the token endpoint answer an OAuth error ("" : none).
func (p *Provider) TokenError(code string) { p.mu.Lock(); p.tokenError = code; p.mu.Unlock() }

// AuthError makes the authorization endpoint send the user back with an
// error such as access_denied ("" : approve).
func (p *Provider) AuthError(code string) { p.mu.Lock(); p.authError = code; p.mu.Unlock() }

// IssParam sends iss on the redirect back (RFC 9207); "" sends none.
func (p *Provider) IssParam(iss string) { p.mu.Lock(); p.issParam = iss; p.mu.Unlock() }

// Seen is the last authorization request.
func (p *Provider) Seen() Authorize { p.mu.Lock(); defer p.mu.Unlock(); return p.seen }

// Issued lists every code, access token and ID token handed out, for
// tests that search outputs for them.
func (p *Provider) Issued() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.issued...)
}

// setNext is Next over HTTP, for scripts driving a running mock: a JSON
// object {"sub", "email", "email_verified"}.
func (p *Provider) setNext(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified *bool  `json:"email_verified"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Sub == "" {
		http.Error(w, "want {\"sub\", \"email\", \"email_verified\"}", http.StatusBadRequest)
		return
	}
	p.Next(Claims{Subject: in.Sub, Email: in.Email, EmailVerified: in.EmailVerified})
	w.WriteHeader(http.StatusNoContent)
}

func (p *Provider) isDown(w http.ResponseWriter) bool {
	p.mu.Lock()
	down := p.down
	p.mu.Unlock()
	if down {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}
	return down
}

func (p *Provider) discovery(w http.ResponseWriter, r *http.Request) {
	if p.isDown(w) {
		return
	}
	writeJSON(w, map[string]any{
		"issuer":                                p.URL,
		"authorization_endpoint":                p.URL + "/authorize",
		"token_endpoint":                        p.URL + "/token",
		"jwks_uri":                              p.URL + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
		"scopes_supported":                      []string{"openid", "email", "profile"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
	})
}

func (p *Provider) jwks(w http.ResponseWriter, r *http.Request) {
	if p.isDown(w) {
		return
	}
	p.mu.Lock()
	var keys []map[string]string
	for _, k := range p.keys {
		keys = append(keys, map[string]string{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": k.kid,
			"n": b64(k.key.N.Bytes()), "e": b64(big.NewInt(int64(k.key.E)).Bytes())})
	}
	p.mu.Unlock()
	writeJSON(w, map[string]any{"keys": keys})
}

func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	a := Authorize{ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"), Scope: q.Get("scope"), State: q.Get("state"),
		Nonce: q.Get("nonce"), Challenge: q.Get("code_challenge"), Method: q.Get("code_challenge_method"), ResponseType: q.Get("response_type")}
	p.mu.Lock()
	p.seen = a
	authError, issParam := p.authError, p.issParam
	p.mu.Unlock()
	if a.ClientID != p.ClientID || a.ResponseType != "code" || a.RedirectURI == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	back, err := url.Parse(a.RedirectURI)
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	bq := back.Query()
	bq.Set("state", a.State)
	if issParam != "" {
		bq.Set("iss", issParam)
	}
	if authError != "" {
		bq.Set("error", authError)
		bq.Set("error_description", "the test provider refused")
	} else {
		c := random()
		p.mu.Lock()
		p.codes[c] = &code{redirect: a.RedirectURI, nonce: a.Nonce, challenge: a.Challenge}
		p.issued = append(p.issued, c)
		p.mu.Unlock()
		bq.Set("code", c)
	}
	back.RawQuery = bq.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	if p.isDown(w) {
		return
	}
	p.mu.Lock()
	delay, tokenError := p.tokenDelay, p.tokenError
	p.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	r.ParseForm()
	id, secret, ok := r.BasicAuth()
	if ok {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != p.ClientID || secret != p.ClientSecret {
		oauthError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	if tokenError != "" {
		oauthError(w, http.StatusBadRequest, tokenError)
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type")
		return
	}
	p.mu.Lock()
	c := p.codes[r.PostForm.Get("code")]
	valid := c != nil && !c.used && c.redirect == r.PostForm.Get("redirect_uri") &&
		c.challenge == challenge(r.PostForm.Get("code_verifier"))
	if c != nil {
		c.used = true
	}
	p.mu.Unlock()
	if !valid {
		oauthError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	idToken := p.idToken(c.nonce)
	access := random()
	p.mu.Lock()
	p.issued = append(p.issued, idToken, access)
	p.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 3600, "id_token": idToken})
}

// idToken signs the next identity's ID token, as the knobs say.
func (p *Provider) idToken(nonce string) string {
	p.mu.Lock()
	c, f, k := p.next, p.forge, p.keys[len(p.keys)-1]
	pub := &p.keys[len(p.keys)-1].key.PublicKey
	p.mu.Unlock()
	now := time.Now()
	iss, aud := p.URL, any(p.ClientID)
	if f.Issuer != "" {
		iss = f.Issuer
	}
	if f.Audience != "" {
		aud = f.Audience
	} else if len(c.Audiences) > 0 {
		aud = append([]string{p.ClientID}, c.Audiences...)
	}
	if f.Nonce != "" {
		nonce = f.Nonce
	}
	iat, exp := now, now.Add(time.Hour)
	if f.Expired {
		iat, exp = now.Add(-2*time.Hour), now.Add(-time.Hour)
	}
	if f.NotYet {
		iat = now.Add(time.Hour)
	}
	claims := map[string]any{"iss": iss, "sub": c.Subject, "aud": aud, "exp": exp.Unix(), "iat": iat.Unix(), "nonce": nonce}
	if f.NotYet {
		claims["nbf"] = iat.Unix()
	}
	if c.Email != "" {
		claims["email"] = c.Email
	}
	if c.EmailVerified != nil {
		claims["email_verified"] = *c.EmailVerified
	}
	if c.XMSEdov != nil {
		claims["xms_edov"] = *c.XMSEdov
	}
	if c.AZP != "" {
		claims["azp"] = c.AZP
	}
	payload, _ := json.Marshal(claims)
	switch {
	case f.AlgNone:
		return jws(map[string]string{"alg": "none", "typ": "JWT"}, payload, func([]byte) []byte { return nil })
	case f.HS256:
		der, _ := x509.MarshalPKIXPublicKey(pub)
		secret := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
		return jws(map[string]string{"alg": "HS256", "typ": "JWT", "kid": k.kid}, payload, func(in []byte) []byte {
			m := hmac.New(sha256.New, secret)
			m.Write(in)
			return m.Sum(nil)
		})
	case f.UnknownKid:
		k = newKey()
	}
	return jws(map[string]string{"alg": "RS256", "typ": "JWT", "kid": k.kid}, payload, func(in []byte) []byte {
		h := sha256.Sum256(in)
		sig, err := rsa.SignPKCS1v15(rand.Reader, k.key, crypto.SHA256, h[:])
		if err != nil {
			panic(err)
		}
		return sig
	})
}

func jws(header map[string]string, payload []byte, sign func([]byte) []byte) string {
	h, _ := json.Marshal(header)
	in := b64(h) + "." + b64(payload)
	return in + "." + b64(sign([]byte(in)))
}

func challenge(verifier string) string {
	s := sha256.Sum256([]byte(verifier))
	return b64(s[:])
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func random() string {
	b := make([]byte, 24)
	rand.Read(b)
	return b64(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%q}`, code)
}

// Ptr is a pointer to v, for the optional claims.
func Ptr[T any](v T) *T { return &v }
