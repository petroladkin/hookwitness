package emulate

import (
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/auth"
)

// A secret imported from a setup file has a name and a kind but no value
// yet: checks that use it are valid, never pass and never refuse.
func TestUnsetSecret(t *testing.T) {
	c := Config{Default: Reply{Status: 200},
		Secrets: []Secret{{Name: "GH", Kind: SecretKey}, {Name: "SW", Kind: SecretKey}, {Name: "TOK", Kind: SecretToken}},
		Checks: []Check{
			{Kind: KindGitHub, Secret: "GH", Enforce: true},
			{Kind: KindStandard, Secret: "SW", Enforce: true},
			{Kind: KindBearer, Secret: "TOK", Enforce: true},
		}}
	if err := c.Validate(time.Minute); err != nil {
		t.Fatalf("validate: %v", err)
	}
	req := func(hs ...string) Request {
		r := Request{Body: []byte(`{}`)}
		for i := 0; i+1 < len(hs); i += 2 {
			r.Headers = append(r.Headers, Header{hs[i], hs[i+1]})
		}
		return r
	}
	for _, s := range c.Secrets {
		if s.Set() {
			t.Fatalf("%s reads as set", s.Name)
		}
	}
	if !signingKey("K", "v").Set() || !token("T", "v").Set() {
		t.Fatal("a secret with a value reads as not set")
	}
	vs, refuse := c.Verify(req("X-Hub-Signature-256", ghSig("x", ""), "Authorization", "Bearer x"), time.Now(), true, keyring{})
	if refuse != nil {
		t.Fatalf("an unset secret refused: %+v", refuse)
	}
	if len(vs) != 3 {
		t.Fatalf("verdicts %+v", vs)
	}
	for _, v := range vs {
		if v.Result != auth.NotVerifiable || !strings.Contains(v.Reason, "not set") {
			t.Errorf("check %d: %s %q, want n/v naming 'not set'", v.Check, v.Result, v.Reason)
		}
	}
	// Without a key ring loaded the reason is still that the secret is not set.
	vs, _ = c.Verify(req(), time.Now(), true, nil)
	if !strings.Contains(vs[0].Reason, "secret GH not set") {
		t.Errorf("reason %q", vs[0].Reason)
	}
	// Still refused: a kind that is not one, a token used as a signing key.
	bad := c
	bad.Secrets = []Secret{{Name: "GH", Kind: "pem"}}
	if err := bad.Validate(time.Minute); err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Errorf("unknown kind: %v", err)
	}
	bad = Config{Default: Reply{Status: 200}, Secrets: []Secret{{Name: "TOK", Kind: SecretToken}},
		Checks: []Check{{Kind: KindGitHub, Secret: "TOK"}}}
	if err := bad.Validate(time.Minute); err == nil {
		t.Error("a token for a GitHub check validated")
	}
	// A value half there (a seal on a token) is damage, not "not set".
	bad = Config{Default: Reply{Status: 200}, Secrets: []Secret{{Name: "TOK", Kind: SecretToken, Sealed: "sealed:x"}}}
	if err := bad.Validate(time.Minute); err == nil {
		t.Error("a token with a seal validated")
	}
}
