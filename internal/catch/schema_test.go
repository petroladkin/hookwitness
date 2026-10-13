package catch

import (
	"net/http"
	"strings"
	"testing"

	"github.com/petroladkin/hookwitness/internal/emulate"
)

func schemaConfig(enforce bool) emulate.Config {
	return emulate.Config{
		Default: emulate.Reply{Status: 202},
		Schemas: []emulate.Schema{{Name: "OrderPaid", Text: `{"type":"object","required":["order_id"],"properties":{"order_id":{"type":"integer"}}}`}},
		Checks: []emulate.Check{{Kind: emulate.KindSchema, Enforce: enforce, Select: &emulate.Select{
			From: emulate.FromBody, Key: "/type", Map: []emulate.Mapping{{Value: "order.paid", Schema: "OrderPaid"}}}}},
	}
}

func TestSchemaVerdictRecordedNotEnforced(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	url := serve(t, h)
	ep := configure(t, st, "/orders", schemaConfig(false))
	resp, _, err := do(t, http.DefaultClient, "POST", url+"/orders", `{"type":"order.paid","order_id":"42"}`)
	if err != nil || resp.StatusCode != 202 {
		t.Fatalf("recording only must not refuse: %v %v", resp, err)
	}
	r, out := lastOutcome(t, st, ep)
	vs := verdictsOf(t, r)
	if r.Verdict != "fail" || vs[0].Total != 1 || vs[0].Problems[0].Path != "/order_id" || vs[0].Problems[0].Keyword != "type" || out.Counter != 1 {
		t.Fatalf("verdict %q %+v, counter %d", r.Verdict, vs, out.Counter)
	}
	do(t, http.DefaultClient, "POST", url+"/orders", `{"type":"order.created"}`)
	if r, _ := lastOutcome(t, st, ep); r.Verdict != "n/c" {
		t.Fatalf("an event without a schema: %q %s", r.Verdict, r.Verdicts)
	}
}

func TestSchemaEnforced(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	url := serve(t, h)
	ep := configure(t, st, "/orders", schemaConfig(true))
	resp, body, err := do(t, http.DefaultClient, "POST", url+"/orders", `{"type":"order.paid"}`)
	if err != nil || resp.StatusCode != 400 || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" ||
		!strings.Contains(body, "JSON Schema by /type: 1 error against OrderPaid") || !strings.Contains(body, "(body): missing property 'order_id'") {
		t.Fatalf("refusal: %v %q %v", resp, body, err)
	}
	r, out := lastOutcome(t, st, ep)
	if out.Action != emulate.ActRefused || out.Counter != 0 || r.Verdict != "fail" {
		t.Fatalf("outcome %+v, verdict %q: a refused request does not count", out, r.Verdict)
	}
	if resp, _, _ := do(t, http.DefaultClient, "POST", url+"/orders", `{"type":"order.paid","order_id":1}`); resp.StatusCode != 202 {
		t.Fatalf("a matching body: %d", resp.StatusCode)
	}
	if _, out := lastOutcome(t, st, ep); out.Counter != 1 {
		t.Fatalf("the first accepted request is number %d", out.Counter)
	}
	if resp, _, _ := do(t, http.DefaultClient, "POST", url+"/orders", `{"type":"order.created"}`); resp.StatusCode != 202 {
		t.Fatalf("an event without a schema is let through: %d", resp.StatusCode)
	}
	if resp, body, _ := do(t, http.DefaultClient, "POST", url+"/orders", `{"type":`); resp.StatusCode != 400 || !strings.Contains(body, "not JSON") {
		t.Fatalf("not JSON: %d %q", resp.StatusCode, body)
	}
}

func TestSchemaBodyNotKept(t *testing.T) {
	h, st, _ := setup(t, 16)
	url := serve(t, h)
	ep := configure(t, st, "/orders", schemaConfig(true))
	resp, _, _ := do(t, http.DefaultClient, "POST", url+"/orders", `{"type":"order.paid","order_id":1,"padding":"xxxxxxxx"}`)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if r, _ := lastOutcome(t, st, ep); r.Verdict != "n/v" {
		t.Fatalf("verdict %q %s", r.Verdict, r.Verdicts)
	}
}

func TestSchemaImageEnforced(t *testing.T) {
	h, st, _ := setup(t, 1<<20)
	url := serve(t, h)
	ep := configure(t, st, "/avatars", emulate.Config{
		Default: emulate.Reply{Status: 202},
		Schemas: []emulate.Schema{{Name: "Avatar", Text: `{"type":"object","required":["image"],"properties":{"image":
			{"type":"string","contentEncoding":"base64","contentMediaType":"image/png"}}}`}},
		Checks: []emulate.Check{{Kind: emulate.KindSchema, Schema: "Avatar", Enforce: true}},
	})
	png1 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg=="
	if resp, _, _ := do(t, http.DefaultClient, "POST", url+"/avatars", `{"image":"`+png1+`"}`); resp.StatusCode != 202 {
		t.Fatalf("a PNG: %d", resp.StatusCode)
	}
	if r, _ := lastOutcome(t, st, ep); !strings.Contains(r.Verdicts, `"images":[{"path":"/image","declared":"image/png","type":"image/png","size":70,"width":1,"height":1`) {
		t.Fatalf("verdict %s", r.Verdicts)
	}
	resp, body, _ := do(t, http.DefaultClient, "POST", url+"/avatars", `{"image":"aGVsbG8="}`)
	if resp.StatusCode != 400 || !strings.Contains(body, "/image: not a valid image/png: not a PNG, JPEG, GIF, WebP, HEIC or AVIF image") {
		t.Fatalf("not an image: %d %q", resp.StatusCode, body)
	}
}
