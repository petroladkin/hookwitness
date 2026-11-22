package oidctest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestProvider(t *testing.T) {
	p := New("hw-client", "hw-secret")
	defer p.Close()
	var d map[string]any
	res, err := http.Get(p.URL + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(res.Body).Decode(&d)
	res.Body.Close()
	if d["issuer"] != p.URL || d["authorization_endpoint"] != p.URL+"/authorize" || d["token_endpoint"] != p.URL+"/token" || d["jwks_uri"] != p.URL+"/jwks" {
		t.Fatalf("discovery: %v", d)
	}
	var jwks struct{ Keys []map[string]string }
	res, _ = http.Get(p.URL + "/jwks")
	json.NewDecoder(res.Body).Decode(&jwks)
	res.Body.Close()
	if len(jwks.Keys) != 1 || jwks.Keys[0]["kid"] != p.Kid() || jwks.Keys[0]["kty"] != "RSA" {
		t.Fatalf("jwks: %v", jwks)
	}

	verifier := "a-verifier-of-at-least-forty-three-characters-0123"
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {"hw-client"}, "response_type": {"code"}, "redirect_uri": {"http://127.0.0.1:9/cb"}, "scope": {"openid email"},
		"state": {"st"}, "nonce": {"nn"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	authorize := func() string {
		t.Helper()
		res, err := noRedirect().Get(p.URL + "/authorize?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		loc, _ := url.Parse(res.Header.Get("Location"))
		if res.StatusCode != http.StatusFound || loc.Query().Get("state") != "st" || loc.Query().Get("code") == "" {
			t.Fatalf("authorize: %d %s", res.StatusCode, loc)
		}
		return loc.Query().Get("code")
	}
	code := authorize()
	if s := p.Seen(); s.Nonce != "nn" || s.Method != "S256" || s.Challenge == "" {
		t.Fatalf("seen %+v", s)
	}
	exchange := func(secret, code, redirect, verifier string) (int, map[string]any) {
		t.Helper()
		f := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "code_verifier": {verifier}}
		req, _ := http.NewRequest("POST", p.URL+"/token", strings.NewReader(f.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth("hw-client", secret)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out
	}
	if st, out := exchange("wrong", code, "http://127.0.0.1:9/cb", verifier); st != 401 || out["error"] != "invalid_client" {
		t.Fatalf("wrong secret: %d %v", st, out)
	}
	if st, _ := exchange("hw-secret", code, "http://127.0.0.1:9/cb", "another-verifier"); st != 400 {
		t.Fatalf("wrong verifier: %d", st)
	}
	code = authorize()
	if st, _ := exchange("hw-secret", code, "http://127.0.0.1:9/other", verifier); st != 400 {
		t.Fatalf("another redirect URI: %d", st)
	}
	code = authorize()
	p.Next(Claims{Subject: "u-1", Email: "ann@example.com", EmailVerified: Ptr(true)})
	st, out := exchange("hw-secret", code, "http://127.0.0.1:9/cb", verifier)
	if st != 200 || out["id_token"] == nil || out["access_token"] == nil {
		t.Fatalf("exchange: %d %v", st, out)
	}
	parts := strings.Split(out["id_token"].(string), ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c map[string]any
	json.Unmarshal(payload, &c)
	if c["sub"] != "u-1" || c["nonce"] != "nn" || c["aud"] != "hw-client" || c["iss"] != p.URL || c["email_verified"] != true {
		t.Fatalf("claims %v", c)
	}
	if st, _ := exchange("hw-secret", code, "http://127.0.0.1:9/cb", verifier); st != 400 {
		t.Fatalf("a used code: %d", st)
	}
	if len(p.Issued()) < 3 {
		t.Fatalf("issued %v", p.Issued())
	}
	p.Down(true)
	if res, _ := http.Get(p.URL + "/.well-known/openid-configuration"); res.StatusCode != 503 {
		t.Fatalf("down: %d", res.StatusCode)
	}
}
