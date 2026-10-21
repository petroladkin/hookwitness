package setup

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/emulate"
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
	if raw["hookwitness"] != float64(Format) {
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
		{"newer", strings.Replace(good, `"hookwitness": 1`, `"hookwitness": 2`, 1), "newer hookwitness (format 2"},
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
