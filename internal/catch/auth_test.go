package catch

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
)

func withKeys(t *testing.T, h *Handler) *secret.Key {
	t.Helper()
	k, err := secret.Load("", t.TempDir()+"/hw.db.key")
	if err != nil {
		t.Fatal(err)
	}
	h.Keys = k
	return k
}

func ghSignature(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func verdictsOf(t *testing.T, r store.Request) []auth.Verdict {
	t.Helper()
	var vs []auth.Verdict
	if err := json.Unmarshal([]byte(r.Verdicts), &vs); err != nil {
		t.Fatalf("verdicts %q: %v", r.Verdicts, err)
	}
	return vs
}

func authConfig(k *secret.Key, enforce bool) emulate.Config {
	return emulate.Config{
		Default: emulate.Reply{Status: 200},
		Secrets: []emulate.Secret{
			{Name: "GH", Kind: emulate.SecretKey, Sealed: k.Seal("gh_test"), Fingerprint: k.Fingerprint("gh_test")},
			{Name: "TOK", Kind: emulate.SecretToken, Hash: k.HashToken("tok_test"), Fingerprint: k.Fingerprint("tok_test")},
		},
		Checks: []emulate.Check{{Kind: emulate.KindGitHub, Secret: "GH", Enforce: enforce}},
	}
}

func TestVerdictRecordedNotEnforced(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	k := withKeys(t, h)
	url := serve(t, h)
	ep := configure(t, st, "/gh", authConfig(k, false))
	resp, _, err := do(t, http.DefaultClient, "POST", url+"/gh", `{"a":1}`, "X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("recording only must not refuse: %v %v", resp, err)
	}
	r, out := lastOutcome(t, st, ep)
	vs := verdictsOf(t, r)
	if r.Verdict != "fail" || len(vs) != 1 || vs[0].Reason != "signature does not match" || vs[0].Expected == "" || vs[0].Received == "" {
		t.Fatalf("verdict %q %+v", r.Verdict, vs)
	}
	if out.Counter != 1 {
		t.Fatalf("counter %d: a recorded-only failure still counts", out.Counter)
	}
	if vs[0].At.Before(r.ReceivedAt) || time.Since(vs[0].At) > time.Minute {
		t.Fatalf("verdict computed at %v, request received %v", vs[0].At, r.ReceivedAt)
	}
}

func TestEnforcedRefusal(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	k := withKeys(t, h)
	url := serve(t, h)
	ep := configure(t, st, "/gh", authConfig(k, true))
	resp, body, _ := do(t, http.DefaultClient, "POST", url+"/gh", `{"a":1}`, "X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	if resp.StatusCode != 401 || body != "Request refused: GitHub signature: signature does not match.\n" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	r, out := lastOutcome(t, st, ep)
	if out.Action != emulate.ActRefused || out.Check != "check 1: GitHub signature: signature does not match" || r.Verdict != "fail" {
		t.Fatalf("%+v %q", out, r.Verdict)
	}
	if got, _ := st.Endpoint(context.Background(), ep); got.Counter != 0 {
		t.Fatalf("counter %d: a refused request must not count", got.Counter)
	}
	resp, _, _ = do(t, http.DefaultClient, "POST", url+"/gh", `{"a":1}`, "X-Hub-Signature-256", ghSignature("gh_test", `{"a":1}`))
	r, out = lastOutcome(t, st, ep)
	if resp.StatusCode != 200 || r.Verdict != "pass" || out.Counter != 1 {
		t.Fatalf("signed request: %d %q %d", resp.StatusCode, r.Verdict, out.Counter)
	}
}

func TestBearerEnforced(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	k := withKeys(t, h)
	url := serve(t, h)
	c := authConfig(k, false)
	c.Checks = []emulate.Check{{Kind: emulate.KindBearer, Secret: "TOK", Enforce: true}}
	configure(t, st, "/b", c)
	resp, body, _ := do(t, http.DefaultClient, "POST", url+"/b", "", "Authorization", "Bearer nope")
	if resp.StatusCode != 401 || resp.Header.Get("WWW-Authenticate") != "Bearer" || !strings.Contains(body, "token does not match") {
		t.Fatalf("%d %v %q", resp.StatusCode, resp.Header, body)
	}
	if resp, _, _ := do(t, http.DefaultClient, "POST", url+"/b", "", "Authorization", "Bearer tok_test"); resp.StatusCode != 200 {
		t.Fatalf("right token: %d", resp.StatusCode)
	}
	c.Checks[0].Error = &emulate.Reply{Status: 403, Body: `{"error":"forbidden"}`}
	configure(t, st, "/b2", c)
	resp, body, _ = do(t, http.DefaultClient, "POST", url+"/b2", "", "Authorization", "Bearer nope")
	if resp.StatusCode != 403 || body != `{"error":"forbidden"}` {
		t.Fatalf("own reply: %d %q", resp.StatusCode, body)
	}
}

func TestTooLargeIsNotVerifiable(t *testing.T) {
	h, st, _ := setup(t, 10)
	k := withKeys(t, h)
	c := authConfig(k, true)
	c.Checks = append(c.Checks, emulate.Check{Kind: emulate.KindBearer, Secret: "TOK"})
	ep := configure(t, st, "/big", c)
	req, _ := http.NewRequest("POST", "/big", strings.NewReader("this body is over ten bytes"))
	req.Header.Set("X-Hub-Signature-256", ghSignature("gh_test", "this body is over ten bytes"))
	req.Header.Set("Authorization", "Bearer tok_test")
	rec := sendRequest(h, req)
	if rec.Code != 413 {
		t.Fatalf("status %d", rec.Code)
	}
	rs, _ := st.Requests(context.Background(), ep, 0, 1)
	vs := verdictsOf(t, rs[0])
	if rs[0].Verdict != "n/v" || vs[0].Result != auth.NotVerifiable || vs[0].Reason != "body not kept (over the size limit)" || vs[1].Result != auth.Pass {
		t.Fatalf("%q %+v", rs[0].Verdict, vs)
	}
}

func TestStripeAgeUsesArrivalTime(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	k := withKeys(t, h)
	url := serve(t, h)
	c := authConfig(k, false)
	c.Secrets[0] = emulate.Secret{Name: "GH", Kind: emulate.SecretKey, Sealed: k.Seal("whsec_test_1"), Fingerprint: "x"}
	c.Checks = []emulate.Check{{Kind: emulate.KindStripe, Secret: "GH"}}
	ep := configure(t, st, "/s", c)
	body := `{"id":"evt_test"}`
	ts := time.Now().Unix() - 400
	m := hmac.New(sha256.New, []byte("whsec_test_1"))
	fmt.Fprintf(m, "%d.%s", ts, body)
	header := fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(m.Sum(nil)))
	do(t, http.DefaultClient, "POST", url+"/s", body, "Stripe-Signature", header)
	r, _ := lastOutcome(t, st, ep)
	vs := verdictsOf(t, r)
	if !strings.HasPrefix(vs[0].Reason, "timestamp 40") || !strings.HasSuffix(vs[0].Reason, " s old (tolerance 300 s)") {
		t.Fatalf("%+v", vs[0])
	}
	c.Checks[0].IgnoreTimestamp = true
	ep2 := configure(t, st, "/s2", c)
	do(t, http.DefaultClient, "POST", url+"/s2", body, "Stripe-Signature", header)
	if r, _ := lastOutcome(t, st, ep2); r.Verdict != "pass" {
		t.Fatalf("ignored timestamp: %q", r.Verdict)
	}
}

func TestNoChecksNoVerdict(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	withKeys(t, h)
	ep := configure(t, st, "/plain", emulate.Config{Default: emulate.Reply{Status: 200}})
	send(h, "POST", "/plain", strings.NewReader("x"))
	r, _ := lastOutcome(t, st, ep)
	if r.Verdict != "" || r.Verdicts != "" {
		t.Fatalf("%q %q", r.Verdict, r.Verdicts)
	}
}

// TestLostKeyKeepsEnforcedEndpointsClosed: with a key that cannot open the
// signing key (the key file was lost and recreated), an enforced check
// refuses even a correctly signed request instead of letting it through.
func TestLostKeyKeepsEnforcedEndpointsClosed(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	k := withKeys(t, h)
	url := serve(t, h)
	ep := configure(t, st, "/gh", authConfig(k, true))
	withKeys(t, h) // a new, different key
	resp, body, _ := do(t, http.DefaultClient, "POST", url+"/gh", `{"a":1}`, "X-Hub-Signature-256", ghSignature("gh_test", `{"a":1}`))
	if resp.StatusCode != 401 || !strings.Contains(body, "cannot be decrypted") {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	if r, _ := lastOutcome(t, st, ep); r.Verdict != "n/v" {
		t.Fatalf("verdict %q", r.Verdict)
	}
}

// A secret imported without its value: the enforced check gives n/v
// "secret GH not set" and lets the request through.
func TestUnsetSecretNeverRefuses(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	withKeys(t, h)
	url := serve(t, h)
	ep := configure(t, st, "/gh", emulate.Config{Default: emulate.Reply{Status: 200},
		Secrets: []emulate.Secret{{Name: "GH", Kind: emulate.SecretKey}},
		Checks:  []emulate.Check{{Kind: emulate.KindGitHub, Secret: "GH", Enforce: true}}})
	resp, _, err := do(t, http.DefaultClient, "POST", url+"/gh", `{"a":1}`, "X-Hub-Signature-256", ghSignature("gh_test", `{"a":1}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("an unset secret refused: %v %v", resp, err)
	}
	r, out := lastOutcome(t, st, ep)
	vs := verdictsOf(t, r)
	if r.Verdict != "n/v" || len(vs) != 1 || vs[0].Reason != "secret GH not set" || out.Counter != 1 {
		t.Fatalf("verdict %q %+v, counter %d", r.Verdict, vs, out.Counter)
	}
}
