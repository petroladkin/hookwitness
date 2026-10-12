package emulate

import (
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/auth"
)

var orderSchemas = []Schema{
	{Name: "OrderPaid", Text: `{"type":"object","required":["order_id","amount"],
		"properties":{"order_id":{"type":"integer"},"amount":{"type":"integer","minimum":1}}}`},
	{Name: "OrderRefunded", Text: `{"type":"object","required":["order_id","reason"]}`},
}

func schemaConfig(ch Check) Config {
	return Config{Default: Reply{Status: 200}, Schemas: orderSchemas, Checks: []Check{ch}}
}

func byType() *Select {
	return &Select{From: FromBody, Key: "/type", Map: []Mapping{
		{Value: "order.paid", Schema: "OrderPaid"}, {Value: "order.refunded", Schema: "OrderRefunded"}}}
}

func verifyOne(t *testing.T, cfg Config, body string, hasBody bool, hdr ...Header) (auth.Verdict, *Failed) {
	t.Helper()
	vs, f := cfg.Verify(Request{Method: "POST", Body: []byte(body), Headers: hdr}, time.Now(), hasBody, nil)
	if len(vs) != 1 {
		t.Fatalf("%d verdicts", len(vs))
	}
	return vs[0], f
}

func TestSchemaCheckVerdicts(t *testing.T) {
	one := schemaConfig(Check{Kind: KindSchema, Schema: "OrderPaid"})
	if v, _ := verifyOne(t, one, `{"order_id":1,"amount":5}`, true); v.Result != auth.Pass || v.Reason != "matches OrderPaid" || v.Label != "JSON Schema OrderPaid" {
		t.Fatalf("pass: %+v", v)
	}
	v, f := verifyOne(t, one, `{"order_id":"x","amount":0}`, true)
	if v.Result != auth.Fail || v.Total != 2 || len(v.Problems) != 2 || v.Reason != "2 errors against OrderPaid" || f != nil {
		t.Fatalf("fail: %+v %v", v, f)
	}
	if v, _ := verifyOne(t, one, `{"order_id":`, true); v.Result != auth.Fail || !strings.Contains(v.Reason, "not JSON") {
		t.Fatalf("not JSON: %+v", v)
	}
	if v, _ := verifyOne(t, one, ``, true); v.Result != auth.Fail || v.Reason != "empty body" {
		t.Fatalf("empty: %+v", v)
	}
	if v, _ := verifyOne(t, one, ``, false); v.Result != auth.NotVerifiable {
		t.Fatalf("body not kept: %+v", v)
	}
}

func TestSchemaCheckDiscriminator(t *testing.T) {
	cfg := schemaConfig(Check{Kind: KindSchema, Select: byType()})
	if v, _ := verifyOne(t, cfg, `{"type":"order.paid","order_id":1,"amount":5}`, true); v.Result != auth.Pass || v.Reason != "matches OrderPaid" {
		t.Fatalf("paid: %+v", v)
	}
	if v, _ := verifyOne(t, cfg, `{"type":"order.refunded","order_id":1}`, true); v.Result != auth.Fail || v.Problems[0].Keyword != "required" {
		t.Fatalf("refunded without reason: %+v", v)
	}
	v, _ := verifyOne(t, cfg, `{"type":"order.created"}`, true)
	if v.Result != auth.NotChecked || v.Reason != `no schema for /type "order.created"` || v.Label != "JSON Schema by /type" {
		t.Fatalf("unmapped value: %+v", v)
	}
	if v, _ := verifyOne(t, cfg, `{"kind":"x"}`, true); v.Result != auth.NotChecked || v.Reason != "no /type in the request" {
		t.Fatalf("missing value: %+v", v)
	}
	if v, _ := verifyOne(t, cfg, `not json`, true); v.Result != auth.Fail {
		t.Fatalf("not JSON with a body discriminator: %+v", v)
	}
	nested := schemaConfig(Check{Kind: KindSchema, Select: &Select{From: FromBody, Key: "/data/0/kind", Map: []Mapping{{Value: "7", Schema: "OrderPaid"}}}})
	if v, _ := verifyOne(t, nested, `{"data":[{"kind":7}],"order_id":1,"amount":1}`, true); v.Result != auth.Pass {
		t.Fatalf("nested pointer to a number: %+v", v)
	}
	hdr := schemaConfig(Check{Kind: KindSchema, Select: &Select{From: FromHeader, Key: "X-Event", Map: []Mapping{{Value: "order.paid", Schema: "OrderPaid"}}}})
	if v, _ := verifyOne(t, hdr, `{"order_id":1,"amount":5}`, true, Header{Name: "x-event", Value: "order.paid"}); v.Result != auth.Pass {
		t.Fatalf("header discriminator: %+v", v)
	}
	if v, _ := verifyOne(t, hdr, `{}`, true); v.Result != auth.NotChecked || v.Reason != "no header X-Event in the request" {
		t.Fatalf("header missing: %+v", v)
	}
}

func TestSchemaCheckEnforced(t *testing.T) {
	cfg := schemaConfig(Check{Kind: KindSchema, Select: byType(), Enforce: true})
	_, f := verifyOne(t, cfg, `{"type":"order.paid","order_id":"x"}`, true)
	if f == nil {
		t.Fatal("an enforced failing check did not refuse")
	}
	r := f.Refusal()
	if r.Status != 400 || !strings.Contains(r.Body, "Request refused: JSON Schema by /type: 2 errors against OrderPaid.\n") ||
		!strings.Contains(r.Body, "/order_id: ") || !strings.Contains(r.Body, "(body): ") {
		t.Fatalf("refusal %d:\n%s", r.Status, r.Body)
	}
	if _, f := verifyOne(t, cfg, `{"type":"order.created"}`, true); f != nil {
		t.Fatalf("a value without a schema was refused: %+v", f)
	}
	own := cfg
	own.Checks = []Check{{Kind: KindSchema, Schema: "OrderPaid", Enforce: true, Error: &Reply{Status: 422, Body: "bad"}}}
	if _, f := verifyOne(t, own, `{}`, true); f == nil || f.Refusal().Status != 422 {
		t.Fatalf("own error reply: %+v", f)
	}
}

func TestOverallWithNotChecked(t *testing.T) {
	v := func(rs ...auth.Result) string {
		var vs []auth.Verdict
		for _, r := range rs {
			vs = append(vs, auth.Verdict{Result: r})
		}
		return Overall(vs)
	}
	for got, want := range map[string]string{
		v():                              "",
		v(auth.NotChecked):               "n/c",
		v(auth.NotChecked, auth.Pass):    "pass",
		v(auth.Pass, auth.NotChecked):    "pass",
		v(auth.Pass, auth.NotVerifiable): "n/v",
		v(auth.NotVerifiable, auth.Fail, auth.Pass): "fail",
		v(auth.NotChecked, auth.Fail):               "fail",
		v(auth.NotChecked, auth.NotVerifiable):      "n/v",
	} {
		if got != want {
			t.Errorf("overall %q, want %q", got, want)
		}
	}
}

func TestSchemaCheckValidation(t *testing.T) {
	for _, tc := range []struct {
		cfg  Config
		want string
	}{
		{Config{Default: Reply{Status: 200}, Checks: []Check{{Kind: KindSchema, Schema: "X"}}}, "add a schema first"},
		{schemaConfig(Check{Kind: KindSchema}), "choose the schema"},
		{schemaConfig(Check{Kind: KindSchema, Schema: "Nope"}), `no schema named "Nope"`},
		{schemaConfig(Check{Kind: KindSchema, Select: &Select{From: FromBody, Key: "type", Map: byType().Map}}), "JSON pointer"},
		{schemaConfig(Check{Kind: KindSchema, Select: &Select{From: FromHeader, Key: "X Bad", Map: byType().Map}}), "not a valid HTTP header name"},
		{schemaConfig(Check{Kind: KindSchema, Select: &Select{From: "query", Key: "/t", Map: byType().Map}}), "by the body or a header"},
		{schemaConfig(Check{Kind: KindSchema, Select: &Select{From: FromBody, Key: "/t"}}), "at least one value"},
		{schemaConfig(Check{Kind: KindSchema, Select: &Select{From: FromBody, Key: "/t", Map: []Mapping{{"a", "OrderPaid"}, {"a", "OrderPaid"}}}}), "mapped twice"},
		{schemaConfig(Check{Kind: KindSchema, Select: &Select{From: FromBody, Key: "/t", Map: []Mapping{{"a", "Ghost"}}}}), `no schema named "Ghost"`},
		// A broken schema is refused at save even before a check uses it.
		{Config{Default: Reply{Status: 200}, Schemas: []Schema{{Name: "Remote", Text: `{"$ref":"https://example.com/s.json"}`}}}, "never fetched"},
	} {
		err := tc.cfg.Validate(time.Minute)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: %v, want %q", tc.cfg.Checks, err, tc.want)
		}
	}
	if err := schemaConfig(Check{Kind: KindSchema, Select: byType(), Enforce: true}).Validate(time.Minute); err != nil {
		t.Fatalf("valid: %v", err)
	}
}
