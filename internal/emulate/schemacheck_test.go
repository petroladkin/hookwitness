package emulate

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"os"
	"slices"
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
	if v, _ := verifyOne(t, cfg, `{"type":"order.paid",`, true); v.Result != auth.Fail || v.Reason != "not JSON: unexpected end of JSON input at byte 20" || v.Problems[0].Keyword != "json" {
		t.Fatalf("not JSON with a body discriminator: %+v", v)
	}
	if v, _ := verifyOne(t, cfg, ` `, true); v.Result != auth.Fail || v.Reason != "empty body" {
		t.Fatalf("empty body with a body discriminator: %+v", v)
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
		{schemaConfig(Check{Kind: KindSchema, Schema: "OrderPaid", Schemas: []string{"OrderRefunded"}}), "not both"},
		{schemaConfig(Check{Kind: KindSchema, Schemas: []string{"OrderPaid", "OrderPaid"}}), "names OrderPaid twice"},
		{schemaConfig(Check{Kind: KindSchema, Schemas: []string{"OrderPaid", "Ghost"}}), `no schema named "Ghost"`},
		{schemaConfig(Check{Kind: KindSchema, Schemas: []string{"OrderPaid"}, Select: byType()}), "come from its mapping"},
		{schemaConfig(Check{Kind: KindSchema, Select: &Select{From: FromBody, Key: "/t", Map: byType().Map, Default: Names{"Ghost"}}}), `no schema named "Ghost"`},
		{schemaConfig(Check{Kind: KindSchema, Select: &Select{From: FromBody, Key: "/t", Map: byType().Map, Default: Names{"OrderPaid", "OrderPaid"}}}), "the default names OrderPaid twice"},
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
	cfg := schemaConfig(Check{Kind: KindSchema, Schemas: []string{"OrderPaid", "OrderRefunded"}})
	if err := cfg.Validate(time.Minute); err != nil || cfg.Checks[0].Label() != "JSON Schema OrderPaid or OrderRefunded" {
		t.Fatalf("any of: %v %q", err, cfg.Checks[0].Label())
	}
	if u := cfg.SchemaUsers("OrderRefunded"); len(u) != 1 {
		t.Fatalf("users of an alternative: %v", u)
	}
	sel := byType()
	sel.Map, sel.Default = sel.Map[:1], Names{"OrderRefunded"}
	if u := schemaConfig(Check{Kind: KindSchema, Select: sel}).SchemaUsers("OrderRefunded"); len(u) != 1 {
		t.Fatalf("users of a default: %v", u)
	}
}

func TestNamesJSON(t *testing.T) {
	for in, want := range map[string]string{`"V1"`: `"V1"`, `["V1"]`: `"V1"`, `["V1","V2"]`: `["V1","V2"]`} {
		var n Names
		if err := json.Unmarshal([]byte(in), &n); err != nil {
			t.Fatal(err)
		}
		if b, _ := json.Marshal(n); string(b) != want {
			t.Errorf("%s: %s, want %s", in, b, want)
		}
	}
	var n Names
	if err := json.Unmarshal([]byte(`5`), &n); err == nil {
		t.Fatal("a number is not a schema name")
	}
}

func TestSchemaCheckImages(t *testing.T) {
	cfg := Config{Default: Reply{Status: 200}, Schemas: []Schema{{Name: "Upload", Text: `{"type":"object","properties":{"photo":
		{"type":"string","contentEncoding":"base64","contentMediaType":"image/*"}}}`}},
		Checks: []Check{{Kind: KindSchema, Schema: "Upload"}}}
	// A 1×1 PNG.
	png1 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=="
	v, _ := verifyOne(t, cfg, `{"photo":"`+png1+`"}`, true)
	if v.Result != auth.Pass || len(v.Images) != 1 || v.Images[0].Type != "image/png" || v.Images[0].Width != 1 {
		t.Fatalf("png: %+v", v)
	}
	heic := "AAAAGGZ0eXBoZWljAAAAAAAAAAAAAAAAAAAAAA=="
	if v, _ := verifyOne(t, cfg, `{"photo":"`+heic+`"}`, true); v.Result != auth.NotVerifiable || !strings.Contains(v.Reason, "cannot be decoded here") {
		t.Fatalf("heic: %+v", v)
	}
	v, _ = verifyOne(t, cfg, `{"photo":"aGVsbG8="}`, true)
	if v.Result != auth.Fail || len(v.Images) != 1 || v.Images[0].Error == "" || v.Problems[0].Path != "/photo" {
		t.Fatalf("text as an image: %+v", v)
	}
	// Too large to decode: checked by its header, n/v with the reason.
	if v, _ := verifyOne(t, cfg, `{"photo":"`+hugePNG()+`"}`, true); v.Result != auth.NotVerifiable ||
		!strings.Contains(v.Reason, "were not decoded: decoding it takes") || v.Images[0].NotChecked == "" {
		t.Fatalf("huge: %+v", v)
	}
}

// hugePNG is a 16-bit RGBA PNG of 4100×4100 zero pixels in base64: a few
// hundred kilobytes that would decode to 134 MB.
func hugePNG() string {
	const w, h = 4100, 4100
	var out bytes.Buffer
	out.WriteString("\x89PNG\r\n\x1a\n")
	chunk := func(kind string, data []byte) {
		binary.Write(&out, binary.BigEndian, uint32(len(data)))
		out.WriteString(kind)
		out.Write(data)
		binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(kind), data...)))
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 16, 6
	chunk("IHDR", ihdr)
	var z bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&z, zlib.BestSpeed)
	row := make([]byte, 1+w*8)
	for range h {
		zw.Write(row)
	}
	zw.Close()
	chunk("IDAT", z.Bytes())
	chunk("IEND", nil)
	return base64.StdEncoding.EncodeToString(out.Bytes())
}

// TestSchemaAlternatives runs testdata/schema/alternatives.json: any of
// several schemas, and a discriminator's default.
func TestSchemaAlternatives(t *testing.T) {
	b, err := os.ReadFile("../../testdata/schema/alternatives.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Schemas []Schema
		Cases   []struct {
			Name    string
			Check   json.RawMessage
			Body    string
			Headers [][2]string
			Want    struct {
				Result       auth.Result
				Reason       string
				ReasonPrefix string `json:"reason_prefix"`
				Problems     []string
			}
		}
	}
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatal(err)
	}
	for _, tc := range file.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			var ch Check
			dec := json.NewDecoder(bytes.NewReader(tc.Check))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&ch); err != nil {
				t.Fatal(err)
			}
			cfg := Config{Default: Reply{Status: 200}, Schemas: file.Schemas, Checks: []Check{ch}}
			if err := cfg.Validate(time.Minute); err != nil {
				t.Fatalf("the check is refused: %v", err)
			}
			var hs []Header
			for _, h := range tc.Headers {
				hs = append(hs, Header{Name: h[0], Value: h[1]})
			}
			v, _ := verifyOne(t, cfg, tc.Body, true, hs...)
			w := tc.Want
			if v.Result != w.Result || (w.Reason != "" && v.Reason != w.Reason) || !strings.HasPrefix(v.Reason, w.ReasonPrefix) {
				t.Fatalf("%s %q, want %s %q%q", v.Result, v.Reason, w.Result, w.Reason, w.ReasonPrefix)
			}
			if w.Problems != nil {
				var paths []string
				for _, p := range v.Problems {
					paths = append(paths, p.Path)
				}
				// The validator's order of problems is not stable; compare as sets.
				slices.Sort(paths)
				want := slices.Sorted(slices.Values(w.Problems))
				if !slices.Equal(paths, want) {
					t.Fatalf("problems at %v, want %v", paths, w.Problems)
				}
			}
		})
	}
}
