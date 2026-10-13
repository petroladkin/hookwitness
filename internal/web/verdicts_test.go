package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/store"
)

func TestExhibitVerdicts(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/v")
	at := time.Date(2026, 10, 11, 20, 13, 2, 0, time.UTC)
	vs := `[{"check":1,"label":"Stripe signature","result":"fail","reason":"timestamp 412 s old (tolerance 300 s)","expected":"t=1,v1=c4bcbd5f…","received":"t=1,v1=00","at":"` + at.Format(time.RFC3339) + `"},` +
		`{"check":2,"label":"Bearer token","result":"pass","reason":"token matches","at":"` + at.Format(time.RFC3339) + `"}]`
	r := e.add(t, ep.ID, "{}", func(r *store.Request) { r.Verdict, r.Verdicts = "fail", vs })
	page := e.do("GET", "/admin/e/"+id(ep.ID)+"/r/"+id(r.ID), nil).Body.String()
	for _, want := range []string{
		"<h2>Verdicts</h2>",
		`<div class="verdict verdict-fail"><div class="vstamp">Stripe signature: fail</div>`,
		"timestamp 412 s old (tolerance 300 s)",
		"timestamp 412 s old (tolerance 300 s)<span class=\"when\"> · check 1 · checked 2026-10-11 20:13:02.000 UTC</span>",
		"<tr><th>expected</th><td>t=1,v1=c4bcbd5f…</td></tr>",
		`<div class="verdict verdict-pass"><div class="vstamp">Bearer token: pass</div>`,
		`data-verdict="fail"`,
		`<span class="tag verdict-fail">checks fail</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	plain := e.add(t, ep.ID, "{}", nil)
	if page := e.do("GET", "/admin/e/"+id(ep.ID)+"/r/"+id(plain.ID), nil).Body.String(); strings.Contains(page, "<h2>Verdicts</h2>") {
		t.Error("a request without verdicts shows a Verdicts section")
	}
}

func signGitHub(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// TestRecheck: requests signed with one secret pass; after the secret is
// replaced, a re-check fails them all, without touching their replies.
func TestRecheck(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, _ := e.st.CreateEndpoint(ctx, "/gh")
	base := "/admin/e/" + id(ep.ID)
	if page := e.do("GET", base, nil).Body.String(); strings.Contains(page, "verdict-counts") {
		t.Fatal("counts shown without authorization checks")
	}
	e.edit(ep.ID, url.Values{"op": {"secret-save"}, "i": {"new"}, "name": {"GH"}, "secret_kind": {"key"}, "value": {"secret_a"}})
	e.edit(ep.ID, url.Values{"op": {"check-save"}, "i": {"new"}, "kind": {"github"}, "secret": {"GH"}})
	for i := range 3 {
		body := `{"n":` + id(int64(i)) + `}`
		e.add(t, ep.ID, body, func(r *store.Request) {
			r.Headers = append(r.Headers, store.Header{Name: "X-Hub-Signature-256", Value: signGitHub("secret_a", body)})
			r.Status, r.Outcome = 202, `{"source":"default","action":"reply","status":202}`
		})
	}
	if rec := e.do("POST", base+"/recheck", url.Values{}); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != base+"?rechecked=3" {
		t.Fatalf("recheck: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	counts, _ := e.st.VerdictCounts(ctx, ep.ID)
	if counts["pass"] != 3 {
		t.Fatalf("with secret A: %v", counts)
	}
	page := e.do("GET", base+"?rechecked=3", nil).Body.String()
	if !strings.Contains(page, `<b class="v-pass">3 pass</b>`) || !strings.Contains(page, "Re-checked 3 requests") || !strings.Contains(page, "Re-check 3</button>") {
		t.Fatal("register lacks counts, notice or the re-check button")
	}

	e.edit(ep.ID, url.Values{"op": {"secret-save"}, "i": {"0"}, "value": {"secret_b"}})
	before := time.Now().UTC().Truncate(time.Millisecond)
	e.do("POST", base+"/recheck", url.Values{})
	reqs, _ := e.st.Requests(ctx, ep.ID, 0, 10)
	for _, r := range reqs {
		vs := parseVerdicts(r.Verdicts)
		if r.Verdict != "fail" || len(vs) != 1 || vs[0].Result != auth.Fail || vs[0].At.Before(before) {
			t.Fatalf("after the secret changed: %q %+v", r.Verdict, vs)
		}
		if r.Status != 202 || !strings.Contains(r.Outcome, `"status":202`) {
			t.Fatalf("a re-check changed the reply: %d %s", r.Status, r.Outcome)
		}
	}
	if rec := e.do("POST", "/admin/e/unmatched/recheck", url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("Unmatched has no checks: %d", rec.Code)
	}
}
