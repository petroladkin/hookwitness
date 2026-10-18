package emulate

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/auth"
)

// Check kinds.
const (
	KindHeader   = "header"   // a required header; always refuses
	KindGitHub   = "github"   // X-Hub-Signature-256
	KindStripe   = "stripe"   // Stripe-Signature
	KindStandard = "standard" // Standard Webhooks and Svix
	KindHMAC     = "hmac"     // any HMAC signature header
	KindBearer   = "bearer"   // Authorization: Bearer
	KindBasic    = "basic"    // Authorization: Basic
	KindSchema   = "schema"   // the body against a JSON Schema
)

// CredentialHeaders names the request headers that carry the credentials
// or signatures the endpoint's checks verify, in check order, each once:
// what a report offers to mask.
func (c Config) CredentialHeaders() []string {
	var out []string
	add := func(names ...string) {
		for _, n := range names {
			if n != "" && !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, n) }) {
				out = append(out, n)
			}
		}
	}
	for _, ch := range c.Checks {
		switch ch.kind() {
		case KindGitHub:
			add("X-Hub-Signature-256")
		case KindStripe:
			add("Stripe-Signature")
		case KindStandard:
			add("webhook-signature", "svix-signature")
		case KindHMAC:
			add(ch.Header)
		case KindBearer, KindBasic:
			add("Authorization")
		}
	}
	return out
}

// AuthKinds in the order the check editor offers them.
var AuthKinds = []string{KindGitHub, KindStripe, KindStandard, KindHMAC, KindBearer, KindBasic}

func knownKind(k string) bool {
	return k == KindHeader || k == KindSchema || slices.Contains(AuthKinds, k)
}

func (ch Check) kind() string {
	if ch.Kind == "" {
		return KindHeader
	}
	return ch.Kind
}

// GivesVerdict reports whether the check records a verdict on every
// request: an authorization or a schema check (not a required header).
func (ch Check) GivesVerdict() bool { return ch.kind() != KindHeader }

// Label names the check for verdicts and messages.
func (ch Check) Label() string {
	switch ch.kind() {
	case KindGitHub:
		return "GitHub signature"
	case KindStripe:
		return "Stripe signature"
	case KindStandard:
		return "Standard Webhooks signature"
	case KindHMAC:
		return "HMAC " + ch.Header
	case KindBearer:
		return "Bearer token"
	case KindBasic:
		return "Basic credentials"
	case KindHeader:
		return "header " + ch.Header
	case KindSchema:
		if ch.Select != nil {
			return "JSON Schema by " + ch.Select.Label()
		}
		return "JSON Schema " + strings.Join(ch.alternatives(), " or ")
	}
	return ch.Kind
}

// usesKey reports whether the check verifies a signature, which needs the
// signing key itself; bearer and basic only compare against a hash.
func (ch Check) usesKey() bool {
	switch ch.kind() {
	case KindGitHub, KindStripe, KindStandard, KindHMAC:
		return true
	}
	return false
}

// Secret kinds.
const (
	SecretKey   = "key"   // a signing key: sealed, opened to verify
	SecretToken = "token" // a token or password: only its hash is kept
)

// Secret is an endpoint secret as stored: never its plain value.
type Secret struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Sealed      string `json:"sealed,omitempty"` // SecretKey
	Hash        string `json:"hash,omitempty"`   // SecretToken
	Fingerprint string `json:"fingerprint"`
	Standard    bool   `json:"standard,omitempty"` // a "whsec_" base64 key, usable by Standard Webhooks checks
}

// Set reports whether the secret has its value. A secret imported from a
// setup file has only its name and kind until someone gives the value;
// checks that use it give n/v "not set" and never refuse.
func (s Secret) Set() bool { return s.Sealed != "" || s.Hash != "" }

// Keyring opens signing keys and matches tokens (internal/secret.Key).
type Keyring interface {
	Open(sealed string) (string, error)
	MatchToken(record, value string) (bool, error) // an error: could not compare now
}

// SecretByName finds a secret.
func (c Config) SecretByName(name string) (Secret, bool) {
	for _, s := range c.Secrets {
		if s.Name == name {
			return s, true
		}
	}
	return Secret{}, false
}

// SecretUsers names the checks that use a secret, for refusing its deletion.
func (c Config) SecretUsers(name string) []string {
	var out []string
	for i, ch := range c.Checks {
		if ch.GivesVerdict() && ch.Secret == name {
			out = append(out, fmt.Sprintf("check %d (%s)", i+1, ch.Label()))
		}
	}
	return out
}

func (c Config) validateSecrets() error {
	seen := map[string]bool{}
	for _, s := range c.Secrets {
		if !varName.MatchString(s.Name) {
			return fmt.Errorf("secret %q: a name is letters, digits and _, not starting with a digit", s.Name)
		}
		if seen[s.Name] {
			return fmt.Errorf("secret %q is defined twice", s.Name)
		}
		seen[s.Name] = true
		switch {
		case s.Kind != SecretKey && s.Kind != SecretToken:
			return fmt.Errorf("secret %q: unknown kind %q", s.Name, s.Kind)
		case s.Kind == SecretKey && s.Hash != "", s.Kind == SecretToken && s.Sealed != "":
			return fmt.Errorf("secret %q: a value of the other kind", s.Name)
		case !s.Set() && (s.Fingerprint != "" || s.Standard):
			return fmt.Errorf("secret %q has no value", s.Name)
		}
	}
	return nil
}

func (c Config) validateAuth(ch Check, maxWait time.Duration) error {
	if !knownKind(ch.kind()) {
		return fmt.Errorf("unknown kind %q", ch.Kind)
	}
	if ch.kind() == KindSchema {
		return c.validateSchemaCheck(ch, maxWait)
	}
	if ch.Secret == "" {
		return errors.New("choose the secret")
	}
	s, ok := c.SecretByName(ch.Secret)
	switch {
	case !ok:
		return fmt.Errorf("secret %s does not exist", ch.Secret)
	case ch.usesKey() && s.Kind != SecretKey:
		return fmt.Errorf("%s is a token or password, not a signing key", s.Name)
	case !ch.usesKey() && s.Kind != SecretToken:
		return fmt.Errorf("%s is a signing key, not a token or password", s.Name)
	}
	switch ch.kind() {
	case KindStandard:
		if s.Set() && !s.Standard {
			return fmt.Errorf(`%s is not a "whsec_" base64 secret`, s.Name)
		}
	case KindHMAC:
		switch {
		case !headerName.MatchString(ch.Header):
			return fmt.Errorf("header name %q is not a valid HTTP header name", ch.Header)
		case !auth.ValidAlgo(ch.Algo):
			return fmt.Errorf("algorithm %s: use sha1, sha256 or sha512", ch.Algo)
		case ch.Encoding != "hex" && ch.Encoding != "base64":
			return errors.New("encoding hex or base64")
		case badHeaderValue(ch.Prefix):
			return errors.New("the prefix has a control character")
		}
		if _, err := parseTemplate(ch.Content); err != nil {
			return fmt.Errorf("signed content: %w", err)
		}
	case KindBasic:
		if ch.User == "" || strings.ContainsAny(ch.User, ": \t") {
			return errors.New("the user name is one word without a colon")
		}
	}
	if (ch.kind() == KindStripe || ch.kind() == KindStandard) && (ch.ToleranceS < 0 || ch.ToleranceS > 86400) {
		return errors.New("tolerance is 1-86400 s")
	}
	if ch.Error != nil {
		if err := ch.Error.validate(maxWait); err != nil {
			return fmt.Errorf("error reply: %w", err)
		}
	}
	return nil
}

// Verify runs the checks in order. Every authorization check gives a verdict,
// whatever the others did; the first failure that refuses — a required
// header, or an authorization check with Enforce that did not pass — is
// returned. received is
// when the request arrived (signed timestamps are judged against it, so a
// re-check later agrees); each verdict records when it was computed.
// hasBody is false for a request whose body was not kept.
func (c Config) Verify(r Request, received time.Time, hasBody bool, keys Keyring) ([]auth.Verdict, *Failed) {
	in := auth.Input{Body: r.Body, HasBody: hasBody, Received: received}
	for _, h := range r.Headers {
		in.Headers = append(in.Headers, auth.Header{Name: h.Name, Value: h.Value})
	}
	var verdicts []auth.Verdict
	var refuse *Failed
	now := time.Now().UTC().Truncate(time.Millisecond)
	for i, ch := range c.Checks {
		if !ch.GivesVerdict() {
			if refuse == nil {
				if f := (Config{Checks: []Check{ch}}).RunChecks(r); f != nil {
					f.Index = i + 1
					refuse = f
				}
			}
			continue
		}
		v := c.verify(ch, r, in, keys)
		v.Check, v.Label, v.At = i+1, ch.Label(), now
		verdicts = append(verdicts, v)
		// An enforced check lets through only what it could verify: a secret it
		// cannot open (a lost key file) must not open the endpoint. A body that
		// was not kept is answered 413 anyway.
		// A check that does not apply (no schema for this event) lets it through.
		// A secret not set yet (an imported setup) never refuses: the check
		// cannot say anything about the request.
		if refuse == nil && ch.Enforce && !c.unset(ch) && v.Result != auth.Pass && v.Result != auth.NotChecked && (v.Result == auth.Fail || hasBody) {
			refuse = &Failed{Index: i + 1, Check: ch, Why: v.Reason, Problems: v.Problems, Total: v.Total}
		}
	}
	return verdicts, refuse
}

func (c Config) verify(ch Check, r Request, in auth.Input, keys Keyring) auth.Verdict {
	if ch.kind() == KindSchema {
		return c.verifySchema(ch, r, in.HasBody)
	}
	s, ok := c.SecretByName(ch.Secret)
	if !ok {
		return auth.Verdict{Result: auth.NotVerifiable, Reason: "secret " + ch.Secret + " does not exist"}
	}
	if !s.Set() {
		return auth.Verdict{Result: auth.NotVerifiable, Reason: "secret " + s.Name + " not set"}
	}
	if keys == nil {
		return auth.Verdict{Result: auth.NotVerifiable, Reason: "no secret key loaded"}
	}
	if !ch.usesKey() {
		var busy error
		match := func(v string) bool {
			ok, err := keys.MatchToken(s.Hash, v)
			if err != nil {
				busy = err
			}
			return ok
		}
		v := auth.Bearer(in, match)
		if ch.kind() == KindBasic {
			v = auth.Basic(in, ch.User, match)
		}
		if busy != nil {
			return auth.Verdict{Result: auth.NotVerifiable, Reason: "not compared: " + busy.Error()}
		}
		return v
	}
	key, err := keys.Open(s.Sealed)
	if err != nil {
		return auth.Verdict{Result: auth.NotVerifiable, Reason: "secret " + s.Name + " " + err.Error()}
	}
	tol := auth.Tolerance{Seconds: ch.ToleranceS, Ignore: ch.IgnoreTimestamp}
	switch ch.kind() {
	case KindGitHub:
		return auth.GitHub(in, key)
	case KindStripe:
		return auth.Stripe(in, key, tol)
	case KindStandard:
		return auth.Standard(in, key, tol)
	}
	content := r.Body
	if ch.Content != "" && in.HasBody {
		// The body plus a reply's worth of room for whatever surrounds it.
		out, err := renderUpTo(ch.Content, NewData(r, nil, 0), len(r.Body)+maxRendered)
		if err != nil {
			return auth.Verdict{Result: auth.Fail, Reason: "signed content: " + err.Error()}
		}
		content = []byte(out)
	}
	return auth.HMAC(in, auth.HMACParams{Header: ch.Header, Algo: ch.Algo, Encoding: ch.Encoding, Prefix: ch.Prefix}, key, content)
}

// unset reports whether an authorization check uses a secret that exists
// without its value.
func (c Config) unset(ch Check) bool {
	if ch.kind() == KindSchema {
		return false
	}
	s, ok := c.SecretByName(ch.Secret)
	return ok && !s.Set()
}

// VerdictsJSON is the stored form of verdicts, "" without any.
func VerdictsJSON(vs []auth.Verdict) string {
	if len(vs) == 0 {
		return ""
	}
	b, _ := json.Marshal(vs)
	return string(b)
}

// Overall is a request's verdict across its checks: fail if any failed,
// else n/v if any could not be verified, else pass if any passed, else n/c
// when none applied; "" without any.
func Overall(vs []auth.Verdict) string {
	rank := map[auth.Result]int{auth.NotChecked: 1, auth.Pass: 2, auth.NotVerifiable: 3, auth.Fail: 4}
	out := auth.Result("")
	for _, v := range vs {
		if rank[v.Result] > rank[out] {
			out = v.Result
		}
	}
	return string(out)
}

// literal makes text safe as a reply template: a reason can quote the
// request (an Authorization scheme), which must never run as a template.
func literal(s string) string { return strings.ReplaceAll(s, "{{", `{{"{{"}}`) }

// authRefusal is the standard reply to a request refused by an
// authorization check: 401, with WWW-Authenticate for bearer and basic.
func (f Failed) authRefusal() Reply {
	if f.Check.kind() == KindSchema {
		return f.schemaRefusal()
	}
	r := Reply{
		Status:  401,
		Headers: []Header{{Name: "Content-Type", Value: "text/plain; charset=utf-8"}},
		Body:    literal("Request refused: " + f.Check.Label() + ": " + f.Why + ".\n"),
	}
	switch f.Check.kind() {
	case KindBearer:
		r.Headers = append(r.Headers, Header{Name: "WWW-Authenticate", Value: "Bearer"})
	case KindBasic:
		r.Headers = append(r.Headers, Header{Name: "WWW-Authenticate", Value: `Basic realm="hookwitness"`})
	}
	return r
}
