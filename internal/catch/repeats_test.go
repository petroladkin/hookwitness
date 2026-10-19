package catch

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
)

const (
	stripeSecret   = "whsec_stripe_test_key"
	standardSecret = "whsec_aG9va3dpdG5lc3Mtc3RhbmRhcmQtdGVzdA==" // base64 of "hookwitness-standard-test"
)

func stripeHeader(body string, t int64) string {
	ts := strconv.FormatInt(t, 10)
	m := hmac.New(sha256.New, []byte(stripeSecret))
	m.Write([]byte(ts + "." + body))
	return "t=" + ts + ",v1=" + hex.EncodeToString(m.Sum(nil))
}

func standardHeaders(body, id string, t int64) []string {
	key, _ := auth.StandardKey(standardSecret)
	ts := strconv.FormatInt(t, 10)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id + "." + ts + "." + body))
	return []string{"webhook-id", id, "webhook-timestamp", ts, "webhook-signature", "v1," + base64.StdEncoding.EncodeToString(m.Sum(nil))}
}

func repeatsConfig(k *secret.Key) emulate.Config {
	return emulate.Config{
		Default: emulate.Reply{Status: 200},
		Secrets: []emulate.Secret{
			{Name: "GH", Kind: emulate.SecretKey, Sealed: k.Seal("gh_test"), Fingerprint: k.Fingerprint("gh_test")},
			{Name: "ST", Kind: emulate.SecretKey, Sealed: k.Seal(stripeSecret), Fingerprint: k.Fingerprint(stripeSecret)},
			{Name: "SW", Kind: emulate.SecretKey, Sealed: k.Seal(standardSecret), Fingerprint: k.Fingerprint(standardSecret), Standard: true},
			{Name: "TOK", Kind: emulate.SecretToken, Hash: k.HashToken("tok_test"), Fingerprint: k.Fingerprint("tok_test")},
		},
	}
}

// noteOf is the note of the newest request's verdict of check n.
func noteOf(t *testing.T, st *store.Store, ep int64, n int) (no int64, result auth.Result, note string) {
	t.Helper()
	r, _ := lastOutcome(t, st, ep)
	for _, v := range verdictsOf(t, r) {
		if v.Check == n {
			return r.No, v.Result, v.Note
		}
	}
	t.Fatalf("request #%d has no verdict of check %d: %s", r.No, n, r.Verdicts)
	return
}

const repeatNote = "a replay, or a redelivery of the same signed request"

// TestRepeatedSignatureNoted: the same signed request sent again carries a
// note naming the first, its result unchanged; a sender's retry that signs
// again does not; a bearer token repeated is not noted.
func TestRepeatedSignatureNoted(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	k := withKeys(t, h)
	url := serve(t, h)
	now := time.Now().Unix()
	body := `{"id":"evt_1"}`
	for _, c := range []struct {
		name, path string
		check      emulate.Check
		first      []string
		resigned   []string
	}{
		{"GitHub", "/gh", emulate.Check{Kind: emulate.KindGitHub, Secret: "GH"},
			[]string{"X-Hub-Signature-256", ghSignature("gh_test", body)}, nil},
		{"Stripe", "/stripe", emulate.Check{Kind: emulate.KindStripe, Secret: "ST"},
			[]string{"Stripe-Signature", stripeHeader(body, now)}, []string{"Stripe-Signature", stripeHeader(body, now+30)}},
		{"Standard Webhooks", "/standard", emulate.Check{Kind: emulate.KindStandard, Secret: "SW"},
			standardHeaders(body, "msg_1", now), standardHeaders(body, "msg_1", now+30)},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := repeatsConfig(k)
			cfg.Checks = []emulate.Check{c.check}
			ep := configure(t, st, c.path, cfg)
			do(t, http.DefaultClient, "POST", url+c.path, body, c.first...)
			if no, res, note := noteOf(t, st, ep, 1); res != auth.Pass || note != "" {
				t.Fatalf("#%d: %s %q, want a pass without a note", no, res, note)
			}
			if c.resigned != nil {
				do(t, http.DefaultClient, "POST", url+c.path, body, c.resigned...)
				if no, res, note := noteOf(t, st, ep, 1); res != auth.Pass || note != "" {
					t.Fatalf("a retry signed again, #%d: %s %q, want a pass without a note", no, res, note)
				}
			}
			do(t, http.DefaultClient, "POST", url+c.path, body, c.first...)
			no, res, note := noteOf(t, st, ep, 1)
			if res != auth.Pass || note != "same signature as #1: "+repeatNote {
				t.Fatalf("sent again as is, #%d: %s %q", no, res, note)
			}
		})
	}
	t.Run("bearer", func(t *testing.T) {
		cfg := repeatsConfig(k)
		cfg.Checks = []emulate.Check{{Kind: emulate.KindBearer, Secret: "TOK"}}
		ep := configure(t, st, "/bearer", cfg)
		for range 2 {
			do(t, http.DefaultClient, "POST", url+"/bearer", body, "Authorization", "Bearer tok_test")
		}
		if _, res, note := noteOf(t, st, ep, 1); res != auth.Pass || note != "" {
			t.Fatalf("%s %q: a repeated token is normal", res, note)
		}
	})
}

// TestRepeatedSignaturePrunedAndRechecked: a request after the first was
// pruned gets no note; a re-check gives the same notes again.
func TestRepeatedSignaturePrunedAndRechecked(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	k := withKeys(t, h)
	url := serve(t, h)
	body := `{"a":1}`
	sig := ghSignature("gh_test", body)
	cfg := repeatsConfig(k)
	cfg.Checks = []emulate.Check{{Kind: emulate.KindGitHub, Secret: "GH"}}
	ep := configure(t, st, "/gh", cfg)
	for range 3 {
		do(t, http.DefaultClient, "POST", url+"/gh", body, "X-Hub-Signature-256", sig)
	}
	ctx := context.Background()
	notes := func() string {
		rs, _ := st.Requests(ctx, ep, 0, 10)
		var out []string
		for i := len(rs) - 1; i >= 0; i-- {
			out = append(out, fmt.Sprintf("#%d %q", rs[i].No, verdictsOf(t, rs[i])[0].Note))
		}
		return strings.Join(out, ", ")
	}
	want := `#1 "", #2 "same signature as #1: ` + repeatNote + `", #3 "same signature as #1: ` + repeatNote + `"`
	if got := notes(); got != want {
		t.Fatalf("notes %s, want %s", got, want)
	}
	e, _ := st.Endpoint(ctx, ep)
	if n, err := Recheck(ctx, st, e, k); err != nil || n != 3 {
		t.Fatalf("re-check %d %v", n, err)
	}
	if got := notes(); got != want {
		t.Fatalf("after a re-check: %s, want %s", got, want)
	}
	// Retention 1: the earlier request goes when the next is kept.
	h.Retention = 1
	do(t, http.DefaultClient, "POST", url+"/gh", body, "X-Hub-Signature-256", sig)
	if _, _, note := noteOf(t, st, ep, 1); note != "" {
		t.Fatalf("note %q naming a pruned request", note)
	}
}
