package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

var opts = Options{Reserved: "/admin", MaxWait: time.Minute}

// sample is an endpoint with every part a setup carries, its secrets set
// (as a running server holds them).
func sample() emulate.Config {
	return emulate.Config{
		Default: emulate.Reply{Status: 202, Headers: []emulate.Header{{Name: "X-Seen", Value: "{{.Var \"ENV\"}}"}}, Body: "ok"},
		Rules: []emulate.Rule{{When: emulate.Condition{Kind: emulate.WhenEvery, N: 3}, Do: emulate.ActReply,
			Reply: emulate.Reply{Status: 503, Body: "try later"}}},
		Checks: []emulate.Check{
			{Header: "X-Env", Match: emulate.MatchPresent},
			{Kind: emulate.KindGitHub, Secret: "GH", Enforce: true},
			{Kind: emulate.KindBearer, Secret: "TOK"},
			{Kind: emulate.KindSchema, Schema: "Order"},
		},
		Vars:    []emulate.Var{{Name: "ENV", Value: "staging"}},
		Schemas: []emulate.Schema{{Name: "Order", Text: `{"type":"object","required":["id"]}`}},
		Secrets: []emulate.Secret{
			{Name: "GH", Kind: emulate.SecretKey, Sealed: "SEALED-gh_test_value", Fingerprint: "fp-gh-1234"},
			{Name: "TOK", Kind: emulate.SecretToken, Hash: "HASHED-tok_test_value", Fingerprint: "fp-tok-5678"},
		},
	}
}

func TestExportCarriesNoSecretValue(t *testing.T) {
	f := Export(Source{Path: "/orders", Config: sample()})
	b := f.JSON()
	for _, bad := range []string{"SEALED", "HASHED", "gh_test_value", "tok_test_value", "fp-gh", "fp-tok", "fingerprint", "sealed", "hash"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("the export contains %q:\n%s", bad, b)
		}
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["hookwitness"] != float64(1) { // nothing newer used: 0.2 reads it
		t.Errorf("format marker %v", raw["hookwitness"])
	}
	if !strings.Contains(string(b), `"name": "GH"`) || !strings.Contains(string(b), `"kind": "token"`) {
		t.Errorf("secret names and kinds missing:\n%s", b)
	}
	if !strings.HasSuffix(string(b), "}\n") {
		t.Error("the file does not end with a newline")
	}
}

func TestRoundTrip(t *testing.T) {
	src := sample()
	f, err := Parse(Export(Source{Path: "/orders", Config: src}).JSON(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Endpoints) != 1 || f.Endpoints[0].Path != "/orders" {
		t.Fatalf("%+v", f)
	}
	got := f.Endpoints[0].Config(nil)
	if err := got.Validate(time.Minute); err != nil {
		t.Fatalf("imported config does not validate: %v", err)
	}
	want := src
	want.Secrets = []emulate.Secret{{Name: "GH", Kind: emulate.SecretKey}, {Name: "TOK", Kind: emulate.SecretToken}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}
	// A second export of the import is the same file.
	if a, b := Export(Source{Path: "/orders", Config: src}).JSON(), Export(Source{Path: "/orders", Config: got}).JSON(); string(a) != string(b) {
		t.Fatalf("export differs after import:\n%s\n%s", a, b)
	}
}

func TestReplaceKeepsMatchingSecrets(t *testing.T) {
	old := sample()
	old.Secrets = append(old.Secrets, emulate.Secret{Name: "GONE", Kind: emulate.SecretKey, Sealed: "SEALED-x", Fingerprint: "fp"})
	file := sample()
	// TOK becomes a signing key in the file; NEW is new.
	file.Secrets = []emulate.Secret{{Name: "GH", Kind: emulate.SecretKey}, {Name: "TOK", Kind: emulate.SecretKey}, {Name: "NEW", Kind: emulate.SecretToken}}
	file.Checks = []emulate.Check{{Kind: emulate.KindGitHub, Secret: "GH"}, {Kind: emulate.KindGitHub, Secret: "TOK"}, {Kind: emulate.KindBearer, Secret: "NEW"}}
	f, err := Parse(Export(Source{Path: "/orders", Config: file}).JSON(), opts)
	if err != nil {
		t.Fatal(err)
	}
	got := f.Endpoints[0].Config(&old)
	want := []emulate.Secret{old.Secrets[0], {Name: "TOK", Kind: emulate.SecretKey}, {Name: "NEW", Kind: emulate.SecretToken}}
	if !reflect.DeepEqual(got.Secrets, want) {
		t.Fatalf("secrets after a replace:\n got %+v\nwant %+v", got.Secrets, want)
	}
	// Applying the file again over the result changes nothing.
	if again := f.Endpoints[0].Config(&got); again.String() != got.String() {
		t.Fatalf("not idempotent:\n%s\n%s", got, again)
	}
}

func TestParseRefuses(t *testing.T) {
	good := string(Export(Source{Path: "/orders", Config: sample()}).JSON())
	cases := []struct{ name, file, want string }{
		{"not json", "hello", "not a hookwitness setup file"},
		{"no marker", `{"endpoints":[]}`, "not a hookwitness setup file"},
		{"marker of another type", `{"hookwitness":"1","endpoints":[]}`, "not a hookwitness setup file"},
		{"newer", strings.Replace(good, `"hookwitness": 1`, `"hookwitness": 3`, 1), "newer hookwitness (format 3"},
		{"bad retention", `{"hookwitness":2,"endpoints":[{"path":"/a","default":{"status":200},"retention":{"normal":{"count":0}}}]}`, "normal.count"},
		{"unknown retention field", `{"hookwitness":2,"endpoints":[{"path":"/a","default":{"status":200},"retention":{"normal":{"keep":3}}}]}`, `unknown field "keep"`},
		{"zero", strings.Replace(good, `"hookwitness": 1`, `"hookwitness": 0`, 1), "format 0"},
		{"unknown top field", strings.Replace(good, `"hookwitness": 1,`, `"hookwitness": 1, "extra": true,`, 1), `unknown field "extra"`},
		{"unknown nested field", strings.Replace(good, `"status": 202`, `"status": 202, "color": "red"`, 1), `unknown field "color"`},
		{"a secret value", strings.Replace(good, `"name": "GH",`, `"name": "GH", "value": "gh_test",`, 1), `unknown field "value"`},
		{"no endpoints", `{"hookwitness":1,"endpoints":[]}`, "no endpoints"},
		{"twice", `{"hookwitness":1,"endpoints":[{"path":"/a","default":{"status":200}},{"path":"/a","default":{"status":200}}]}`, "/a is in the file twice"},
		{"bad path", `{"hookwitness":1,"endpoints":[{"path":"a","default":{"status":200}}]}`, `endpoint "a": The path must start`},
		{"reserved path", `{"hookwitness":1,"endpoints":[{"path":"/admin/x","default":{"status":200}}]}`, "reserved"},
		{"invalid behaviour", `{"hookwitness":1,"endpoints":[{"path":"/a","default":{"status":200},"checks":[{"kind":"github","secret":"NOPE"}]}]}`, "endpoint /a: check 1 (GitHub signature): secret NOPE does not exist"},
		{"trailing", good + "{}", "after the setup"},
	}
	for _, c := range cases {
		_, err := Parse([]byte(c.file), opts)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

func TestFileName(t *testing.T) {
	for in, want := range map[string]string{
		"/orders":          "orders.hookwitness.json",
		"/stripe/webhooks": "stripe-webhooks.hookwitness.json",
		"/":                "root.hookwitness.json",
		"/a.b~c_d/":        "a.b-c_d.hookwitness.json",
	} {
		if got := FileName(in); got != want {
			t.Errorf("FileName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRetentionInSetup: an endpoint's own retention travels in format 2
// and is applied at import, pruning at once; a file without it gives the
// defaults back.
func TestRetentionInSetup(t *testing.T) {
	ctx := context.Background()
	var p store.Retention
	if err := json.Unmarshal([]byte(`{"normal":{"count":3,"max_age":"36h"},"anomalies":{"count":0}}`), &p); err != nil {
		t.Fatal(err)
	}
	f := Export(Source{Path: "/keep", Config: emulate.Config{Default: emulate.Reply{Status: 200}}, Retention: p})
	b := f.JSON()
	if f.Hookwitness != 2 || !strings.Contains(string(b), `"retention": {`) || !strings.Contains(string(b), `"max_age": "36h"`) ||
		!strings.Contains(string(b), `"anomalies": {`) {
		t.Fatalf("export with retention (format %d):\n%s", f.Hookwitness, b)
	}
	opts := Options{Reserved: "/admin", MaxWait: time.Minute, Keep: store.Keep{Normal: 1000, Anomalies: 1000}}
	parsed, err := Parse(b, opts)
	if err == nil && (parsed.Endpoints[0].Retention == nil || *parsed.Endpoints[0].Retention != p) {
		t.Fatalf("parsed retention %+v", parsed.Endpoints[0].Retention)
	}
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "hw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ep, _ := st.CreateEndpoint(ctx, "/keep")
	for i := range 10 {
		r := &store.Request{EndpointID: ep.ID, ReceivedAt: time.Now(), Method: "POST", Path: "/keep", Body: []byte(strconv.Itoa(i)), Size: 1, Status: 200}
		if err := st.AddRequest(ctx, r, opts.Keep, nil); err != nil {
			t.Fatal(err)
		}
	}
	res, err := Apply(ctx, st, parsed, Plan{Replace: Always}, opts)
	if err != nil || res[0].Result != Replaced || res[0].Pruned != 7 {
		t.Fatalf("import: %+v %v", res, err)
	}
	got, _ := st.Endpoint(ctx, ep.ID)
	if got.Count != 3 || got.Kept.Policy != p {
		t.Fatalf("after the import: %d kept, policy %+v", got.Count, got.Kept.Policy)
	}
	if res, _ := Apply(ctx, st, parsed, Plan{Replace: Always}, opts); res[0].Result != Unchanged || res[0].Pruned != 0 {
		t.Fatalf("the same file again: %+v", res)
	}
	plain, _ := Parse(Export(Source{Path: "/keep", Config: emulate.Config{Default: emulate.Reply{Status: 200}}}).JSON(), opts)
	if res, _ := Apply(ctx, st, plain, Plan{Replace: Always}, opts); res[0].Result != Replaced {
		t.Fatalf("a file without retention: %+v", res)
	}
	if got, _ := st.Endpoint(ctx, ep.ID); !got.Kept.Policy.IsZero() {
		t.Fatalf("a file without retention left %+v", got.Kept.Policy)
	}
}

// TestDeliveryInSetup: the event source and the delivery checks travel in
// format 2; nothing new used stays format 1.
func TestDeliveryInSetup(t *testing.T) {
	cfg := emulate.Config{Default: emulate.Reply{Status: 200}, Event: &emulate.Source{From: emulate.FromHeader, Key: "X-GitHub-Delivery"},
		Checks: []emulate.Check{{Kind: emulate.KindBackoff, FirstS: 5}}}
	f := Export(Source{Path: "/gh", Config: cfg})
	b := f.JSON()
	if f.Hookwitness != 2 || !strings.Contains(string(b), `"event": {`) || !strings.Contains(string(b), `"kind": "backoff"`) {
		t.Fatalf("format %d:\n%s", f.Hookwitness, b)
	}
	parsed, err := Parse(b, opts)
	if err != nil {
		t.Fatal(err)
	}
	got := parsed.Endpoints[0].Config(nil)
	if got.Event == nil || *got.Event != *cfg.Event || len(got.Checks) != 1 || got.Checks[0].FirstS != 5 {
		t.Fatalf("round trip: %+v", got)
	}
	if f := Export(Source{Path: "/x", Config: emulate.Config{Default: emulate.Reply{Status: 200}, Checks: []emulate.Check{{Kind: emulate.KindDuplicates}}}}); f.Hookwitness != 2 {
		t.Fatalf("a delivery check alone: format %d", f.Hookwitness)
	}
}

// TestClockRulesInSetup: rules by time and state travel in format 2 and
// come back the same; the clock and the rules' state are not part of a
// setup file.
func TestClockRulesInSetup(t *testing.T) {
	rules := []emulate.Rule{
		{When: emulate.Condition{Kind: emulate.WhenWindow, ToS: 1.5}, Do: emulate.ActDrop},
		{When: emulate.Condition{Kind: emulate.WhenFlap, Down: 1, Up: 2, Unit: "requests"}, Do: emulate.ActTimeout},
		{When: emulate.Condition{Kind: emulate.WhenRate, N: 5, PerS: 10, BlockS: 30}, Do: emulate.ActReply,
			Reply: emulate.Reply{Status: 429, Headers: []emulate.Header{{Name: "Retry-After", Value: "{{.RetryAfter}}"}}}},
		{When: emulate.Condition{Kind: emulate.WhenAttempts, N: 2}, Do: emulate.ActReply, Reply: emulate.Reply{Status: 503}},
	}
	for i, r := range rules {
		cfg := emulate.Config{Default: emulate.Reply{Status: 200}, Rules: []emulate.Rule{{When: emulate.Condition{Kind: emulate.WhenEvery, N: 2}, Do: emulate.ActDrop}, r}}
		f := Export(Source{Path: "/clock", Config: cfg})
		b := f.JSON()
		if f.Hookwitness != 2 || strings.Contains(string(b), "start") || strings.Contains(string(b), "state") {
			t.Fatalf("rule %d: format %d:\n%s", i, f.Hookwitness, b)
		}
		parsed, err := Parse(b, opts)
		if err != nil {
			t.Fatal(err)
		}
		if got := parsed.Endpoints[0].Config(nil); got.String() != cfg.String() {
			t.Fatalf("rule %d round trip:\n%s\nwant\n%s", i, got.String(), cfg.String())
		}
	}
	if f := Export(Source{Path: "/count", Config: emulate.Config{Default: emulate.Reply{Status: 200},
		Rules: []emulate.Rule{{When: emulate.Condition{Kind: emulate.WhenFirst, N: 2}, Do: emulate.ActDrop}}}}); f.Hookwitness != 1 {
		t.Fatalf("rules by count only: format %d", f.Hookwitness)
	}
	bad := `{"hookwitness":2,"endpoints":[{"path":"/w","default":{"status":200},"rules":[{"when":{"kind":"window","from_s":5,"to_s":5},"do":"drop"}],"checks":[],"vars":[],"schemas":[],"secrets":[]}]}`
	if _, err := Parse([]byte(bad), opts); err == nil || !strings.Contains(err.Error(), "to_s must be above from_s") {
		t.Fatalf("an empty window: %v", err)
	}
}

// An export that uses nothing newer than 0.2 is what 0.2.0 wrote, byte for
// byte, so 0.2 reads it: testdata/release-0.2/export.json was written by the
// v0.2.0 code from config.json, an endpoint with every part 0.2 knew.
func TestExportAsRelease02(t *testing.T) {
	b, err := os.ReadFile("testdata/release-0.2/config.json")
	if err != nil {
		t.Fatal(err)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var c emulate.Config
	if err := d.Decode(&c); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(time.Minute); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/release-0.2/export.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := Export(Source{Path: "/orders", Config: c}).JSON(); !bytes.Equal(got, want) {
		t.Fatalf("the export differs from 0.2.0's:\n%s", got)
	}
}

// TestForwardInSetup: a receiver travels as configured in format 2 and
// comes back the same, the rules' forward switch with it; a receiver with a
// query is refused at parse.
func TestForwardInSetup(t *testing.T) {
	cfg := emulate.Config{Default: emulate.Reply{Status: 202},
		Forward: &emulate.Forward{URL: "https://receiver.test/in", KeepHost: true, TimeoutMs: 5000, OnFailure: emulate.OnFailureDefault},
		Rules:   []emulate.Rule{{When: emulate.Condition{Kind: emulate.WhenEvery, N: 3}, Do: emulate.ActDrop, Forward: true}}}
	f := Export(Source{Path: "/live", Config: cfg})
	b := f.JSON()
	if f.Hookwitness != 2 || !strings.Contains(string(b), `"url": "https://receiver.test/in"`) {
		t.Fatalf("format %d:\n%s", f.Hookwitness, b)
	}
	parsed, err := Parse(b, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Endpoints[0].Config(nil); got.String() != cfg.String() {
		t.Fatalf("round trip:\n%s\nwant\n%s", got.String(), cfg.String())
	}
	// The rule's switch alone is format 2.
	sw := emulate.Config{Default: emulate.Reply{Status: 200}, Rules: []emulate.Rule{{When: emulate.Condition{Kind: emulate.WhenFirst, N: 1}, Do: emulate.ActDrop, Forward: true}}}
	if f := Export(Source{Path: "/x", Config: sw}); f.Hookwitness != 2 {
		t.Fatalf("a rule's switch: format %d", f.Hookwitness)
	}
	bad := `{"hookwitness":2,"endpoints":[{"path":"/q","default":{"status":200},"forward":{"url":"https://receiver.test/in?token=1"},"rules":[],"checks":[],"vars":[],"schemas":[],"secrets":[]}]}`
	if _, err := Parse([]byte(bad), opts); err == nil || !strings.Contains(err.Error(), "must not have a query") {
		t.Fatalf("a receiver with a query: %v", err)
	}
}
