package emulate

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/auth"
)

// keyring stands in for secret.Key: "sealed:" + value, "hash:" + value.
type keyring struct{}

func (keyring) Open(sealed string) (string, error) {
	v, ok := strings.CutPrefix(sealed, "sealed:")
	if !ok {
		return "", errors.New("cannot be decrypted (wrong secret key?)")
	}
	return v, nil
}
func (keyring) MatchToken(record, value string) (bool, error) { return record == "hash:"+value, nil }

func signingKey(name, value string) Secret {
	_, err := auth.StandardKey(value)
	return Secret{Name: name, Kind: SecretKey, Sealed: "sealed:" + value, Fingerprint: "f1f2f3f4", Standard: err == nil}
}

func token(name, value string) Secret {
	return Secret{Name: name, Kind: SecretToken, Hash: "hash:" + value, Fingerprint: "a1a2a3a4"}
}

func ghSig(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestOldChecksReadAsHeaderChecks(t *testing.T) {
	c, err := Parse(`{"default":{"status":200},"checks":[{"header":"X-Env","match":"equals","value":"test"}]}`)
	if err != nil || len(c.Checks) != 1 || c.Checks[0].kind() != KindHeader || c.Validate(time.Minute) != nil {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestAuthCheckValidate(t *testing.T) {
	base := Config{Default: Reply{Status: 200}, Secrets: []Secret{
		signingKey("GH", "gh_test"), signingKey("SW", "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"), token("TOK", "tok_test"),
	}}
	ok := []Check{
		{Kind: KindGitHub, Secret: "GH"},
		{Kind: KindStripe, Secret: "GH", ToleranceS: 600},
		{Kind: KindStandard, Secret: "SW", IgnoreTimestamp: true},
		{Kind: KindHMAC, Secret: "GH", Header: "X-Signature", Algo: "sha512", Encoding: "base64", Content: `{{.Header "X-Id"}}.{{.Body}}`},
		{Kind: KindBearer, Secret: "TOK", Enforce: true},
		{Kind: KindBasic, User: "qa", Secret: "TOK"},
	}
	for _, ch := range ok {
		c := base
		c.Checks = []Check{ch}
		if err := c.Validate(time.Minute); err != nil {
			t.Errorf("%s: %v", ch.kind(), err)
		}
	}
	for want, ch := range map[string]Check{
		"check 1 (GitHub signature): choose the secret":                             {Kind: KindGitHub},
		"check 1 (GitHub signature): secret NOPE does not exist":                    {Kind: KindGitHub, Secret: "NOPE"},
		"check 1 (GitHub signature): TOK is a token or password, not a signing key": {Kind: KindGitHub, Secret: "TOK"},
		"check 1 (Bearer token): GH is a signing key, not a token or password":      {Kind: KindBearer, Secret: "GH"},
		`check 1 (Standard Webhooks signature): GH is not a "whsec_" base64 secret`: {Kind: KindStandard, Secret: "GH"},
		"check 1 (Stripe signature): tolerance is 1-86400 s":                        {Kind: KindStripe, Secret: "GH", ToleranceS: 90000},
		"check 1 (HMAC X-Signature): algorithm md5: use sha1, sha256 or sha512":     {Kind: KindHMAC, Secret: "GH", Header: "X-Signature", Algo: "md5", Encoding: "hex"},
		"check 1 (HMAC X-Signature): encoding hex or base64":                        {Kind: KindHMAC, Secret: "GH", Header: "X-Signature", Algo: "sha1", Encoding: "b32"},
		`check 1 (HMAC ): header name "" is not a valid HTTP header name`:           {Kind: KindHMAC, Secret: "GH", Algo: "sha1", Encoding: "hex"},
		"check 1 (HMAC X-S): signed content: template error":                        {Kind: KindHMAC, Secret: "GH", Header: "X-S", Algo: "sha1", Encoding: "hex", Content: "{{.Body"},
		"check 1 (Basic credentials): the user name is one word without a colon":    {Kind: KindBasic, Secret: "TOK", User: "a:b"},
		`check 1: unknown kind "jwt"`:                                               {Kind: "jwt"},
	} {
		c := base
		c.Checks = []Check{ch}
		if err := c.Validate(time.Minute); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
	for want, s := range map[string][]Secret{
		`secret "1A": a name is letters, digits and _, not starting with a digit`: {{Name: "1A", Kind: SecretKey, Sealed: "x"}},
		`secret "A" is defined twice`: {{Name: "A", Kind: SecretKey, Sealed: "x"}, {Name: "A", Kind: SecretToken, Hash: "y"}},
		`secret "A": unknown kind ""`: {{Name: "A"}},
	} {
		c := Config{Default: Reply{Status: 200}, Secrets: s}
		if err := c.Validate(time.Minute); err == nil || err.Error() != want {
			t.Errorf("want %q, got %v", want, err)
		}
	}
}

func TestSecretUsers(t *testing.T) {
	c := Config{Checks: []Check{{Header: "X", Match: MatchPresent}, {Kind: KindStripe, Secret: "S"}, {Kind: KindBearer, Secret: "S"}}}
	if got := strings.Join(c.SecretUsers("S"), "; "); got != "check 2 (Stripe signature); check 3 (Bearer token)" {
		t.Fatalf("%q", got)
	}
	if c.SecretUsers("T") != nil {
		t.Fatal("unused secret has users")
	}
}

func TestSecretsAreNotTemplateData(t *testing.T) {
	c := Config{Vars: []Var{{"A", "1"}}, Secrets: []Secret{signingKey("GH", "gh_test")}}
	d := NewData(Request{}, c.Vars, 0)
	out, err := Reply{Status: 200, Body: "{{.Var.A}}|{{.Var.GH}}"}.Render(d)
	if err != nil || string(out.Body) != "1|" {
		t.Fatalf("%q %v", out.Body, err)
	}
	if s := c.String(); strings.Contains(s, `"gh_test"`) {
		t.Fatalf("stored config holds a plain secret: %s", s)
	}
}

func TestVerify(t *testing.T) {
	body := `{"action":"opened"}`
	c := Config{
		Secrets: []Secret{signingKey("GH", "gh_test"), token("TOK", "tok_test")},
		Checks: []Check{
			{Header: "X-Env", Match: MatchPresent},
			{Kind: KindGitHub, Secret: "GH", Enforce: true},
			{Kind: KindBearer, Secret: "TOK"},
		},
	}
	at := time.Date(2026, 10, 11, 20, 0, 0, 0, time.UTC)
	req := func(hs ...string) Request {
		r := Request{Body: []byte(body)}
		for i := 0; i+1 < len(hs); i += 2 {
			r.Headers = append(r.Headers, Header{hs[i], hs[i+1]})
		}
		return r
	}
	good := ghSig("gh_test", body)

	vs, refuse := c.Verify(req("X-Env", "1", "X-Hub-Signature-256", good, "Authorization", "Bearer wrong"), at, true, keyring{})
	if refuse != nil || len(vs) != 2 {
		t.Fatalf("recording-only failure refused: %+v %+v", refuse, vs)
	}
	if vs[0].Check != 2 || vs[0].Label != "GitHub signature" || vs[0].Result != auth.Pass || time.Since(vs[0].At) > time.Minute ||
		vs[1].Check != 3 || vs[1].Result != auth.Fail || vs[1].Reason != "token does not match" {
		t.Fatalf("verdicts %+v", vs)
	}
	if Overall(vs) != "fail" {
		t.Fatal("overall")
	}

	// The enforcing GitHub check refuses; the bearer verdict is still recorded.
	vs, refuse = c.Verify(req("X-Env", "1", "X-Hub-Signature-256", "sha256=00", "Authorization", "Bearer tok_test"), at, true, keyring{})
	if refuse == nil || refuse.Index != 2 || refuse.Describe() != `check 2: GitHub signature: header X-Hub-Signature-256 is not "sha256=<hex>"` || len(vs) != 2 || vs[1].Result != auth.Pass {
		t.Fatalf("enforced: %+v %+v", refuse, vs)
	}
	r := refuse.Refusal()
	if r.Status != 401 || !strings.Contains(r.Body, "GitHub signature: header X-Hub-Signature-256") || hasHeader(r, "WWW-Authenticate") {
		t.Fatalf("refusal %+v", r)
	}

	// A header check fails first in list order: it refuses; verdicts are still all recorded.
	vs, refuse = c.Verify(req("X-Hub-Signature-256", "sha256=00"), at, true, keyring{})
	if refuse == nil || refuse.Index != 1 || len(vs) != 2 {
		t.Fatalf("header first: %+v %+v", refuse, vs)
	}

	// Enforcing bearer: 401 with WWW-Authenticate; own error reply wins.
	c.Checks[2].Enforce = true
	_, refuse = c.Verify(req("X-Env", "1", "X-Hub-Signature-256", good), at, true, keyring{})
	if r := refuse.Refusal(); refuse.Index != 3 || r.Status != 401 || !hasHeader(r, "WWW-Authenticate") {
		t.Fatalf("bearer refusal %+v", r)
	}
	c.Checks[2].Error = &Reply{Status: 403, Body: "go away"}
	_, refuse = c.Verify(req("X-Env", "1", "X-Hub-Signature-256", good), at, true, keyring{})
	if r := refuse.Refusal(); r.Status != 403 || r.Body != "go away" {
		t.Fatalf("own refusal %+v", r)
	}

	// No body kept: GitHub is not verifiable and does not refuse; bearer still decides.
	vs, refuse = c.Verify(Request{Headers: []Header{{"X-Env", "1"}, {"Authorization", "Bearer tok_test"}}}, at, false, keyring{})
	if refuse != nil || vs[0].Result != auth.NotVerifiable || vs[1].Result != auth.Pass || Overall(vs) != "n/v" {
		t.Fatalf("no body: %+v %+v", refuse, vs)
	}

	// A signing key that cannot be opened: not verifiable, with the reason;
	// enforced, that refuses (a lost key must not open the endpoint).
	c.Secrets[0].Sealed = "garbled"
	vs, refuse = c.Verify(req("X-Env", "1", "X-Hub-Signature-256", good, "Authorization", "Bearer tok_test"), at, true, keyring{})
	if vs[0].Result != auth.NotVerifiable || vs[0].Reason != "secret GH cannot be decrypted (wrong secret key?)" {
		t.Fatalf("unreadable secret: %+v", vs[0])
	}
	if refuse == nil || refuse.Index != 2 || refuse.Refusal().Status != 401 {
		t.Fatalf("an enforced check that cannot verify must refuse: %+v", refuse)
	}
	c.Checks[1].Enforce = false
	if _, refuse = c.Verify(req("X-Env", "1", "X-Hub-Signature-256", good, "Authorization", "Bearer tok_test"), at, true, keyring{}); refuse != nil {
		t.Fatalf("record only must not refuse: %+v", refuse)
	}
}

func TestVerifyHMACContentTemplate(t *testing.T) {
	body := `{"n":1}`
	m := hmac.New(sha256.New, []byte("k_test"))
	m.Write([]byte("evt_9." + body))
	sig := base64.StdEncoding.EncodeToString(m.Sum(nil))
	c := Config{Secrets: []Secret{signingKey("K", "k_test")}, Checks: []Check{
		{Kind: KindHMAC, Secret: "K", Header: "X-Sig", Algo: "sha256", Encoding: "base64", Content: `{{.Header "X-Id"}}.{{.Body}}`},
	}}
	r := Request{Body: []byte(body), Headers: []Header{{"X-Id", "evt_9"}, {"X-Sig", sig}}}
	vs, _ := c.Verify(r, time.Now(), true, keyring{})
	if vs[0].Result != auth.Pass || vs[0].Label != "HMAC X-Sig" {
		t.Fatalf("%+v", vs[0])
	}
}

func TestOverall(t *testing.T) {
	v := func(rs ...auth.Result) []auth.Verdict {
		var out []auth.Verdict
		for _, r := range rs {
			out = append(out, auth.Verdict{Result: r})
		}
		return out
	}
	for want, vs := range map[string][]auth.Verdict{
		"":     nil,
		"pass": v(auth.Pass, auth.Pass),
		"n/v":  v(auth.Pass, auth.NotVerifiable),
		"fail": v(auth.NotVerifiable, auth.Fail, auth.Pass),
	} {
		if got := Overall(vs); got != want {
			t.Errorf("%v: %q, want %q", vs, got, want)
		}
	}
}

func hasHeader(r Reply, name string) bool {
	for _, h := range r.Headers {
		if strings.EqualFold(h.Name, name) {
			return true
		}
	}
	return false
}

func TestRefusalDoesNotRunRequestText(t *testing.T) {
	c := Config{Vars: []Var{{"A", "leak"}}, Secrets: []Secret{token("TOK", "tok_test")},
		Checks: []Check{{Kind: KindBearer, Secret: "TOK", Enforce: true}}}
	r := Request{Headers: []Header{{"Authorization", "{{.Var.A}} x"}}}
	_, f := c.Verify(r, time.Now(), true, keyring{})
	out, err := f.Refusal().Render(NewData(r, c.Vars, 0))
	if err != nil || strings.Contains(string(out.Body), "leak") || !strings.Contains(string(out.Body), "scheme is {{.Var.A}}, not Bearer") {
		t.Fatalf("%q %v", out.Body, err)
	}
}

// TestSignedContentOfALargeBody: the content template holds the whole body,
// which may be over a reply's 1 MiB cap (found in review).
func TestSignedContentOfALargeBody(t *testing.T) {
	body := strings.Repeat("x", 2<<20)
	m := hmac.New(sha256.New, []byte("k_test"))
	m.Write([]byte("evt_9." + body))
	sig := hex.EncodeToString(m.Sum(nil))
	c := Config{Secrets: []Secret{signingKey("K", "k_test")}, Checks: []Check{
		{Kind: KindHMAC, Secret: "K", Header: "X-Sig", Algo: "sha256", Encoding: "hex", Content: `{{.Header "X-Id"}}.{{.Body}}`},
	}}
	vs, _ := c.Verify(Request{Body: []byte(body), Headers: []Header{{"X-Id", "evt_9"}, {"X-Sig", sig}}}, time.Now(), true, keyring{})
	if vs[0].Result != auth.Pass {
		t.Fatalf("%+v", vs[0])
	}
}

func TestStampWhy(t *testing.T) {
	for want, o := range map[string]Outcome{
		"check 1: Stripe signature":     {Source: FromCheck, Check: "check 1: Stripe signature: timestamp 412 s old (tolerance 300 s)"},
		"check 2: header X-Env missing": {Source: FromCheck, Check: "check 2: header X-Env missing"},
		"rule 1 · first 2":              {Source: FromRule, Rule: 1, Label: "first 2"},
	} {
		if got := o.StampWhy(); got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}

func TestCredentialHeaders(t *testing.T) {
	c := Config{Checks: []Check{
		{Kind: KindHeader, Header: "X-Required"},
		{Kind: KindStandard},
		{Kind: KindHMAC, Header: "X-Signature"},
		{Kind: KindBearer},
		{Kind: KindBasic},
		{Kind: KindHMAC, Header: "x-signature"},
		{Kind: KindGitHub},
		{Kind: KindSchema, Schema: "A"},
	}}
	got := strings.Join(c.CredentialHeaders(), ",")
	if got != "webhook-signature,svix-signature,X-Signature,Authorization,X-Hub-Signature-256" {
		t.Fatalf("%s", got)
	}
}
