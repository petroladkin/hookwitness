package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/petroladkin/hookwitness/internal/emulate"
)

// behaviour reads an endpoint's behaviour and its ETag.
func (e env) behaviour(t *testing.T, id int64) (behaviourJSON, string) {
	t.Helper()
	rec := e.do("GET", fmt.Sprintf("/endpoints/%d/behaviour", id), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get behaviour: %d %s", rec.Code, rec.Body)
	}
	return decodeAs[behaviourJSON](t, rec), rec.Header().Get("ETag")
}

func (e env) putBehaviour(id int64, etag, body string) *httptest.ResponseRecorder {
	return e.do("PUT", fmt.Sprintf("/endpoints/%d/behaviour", id), body, "If-Match", etag)
}

func (e env) stored(t *testing.T, id int64) emulate.Config {
	t.Helper()
	ep, err := e.st.Endpoint(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := emulate.Parse(ep.Config)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestGetBehaviour(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	rec := e.do("GET", "/endpoints/1/behaviour", "")
	want := `{
  "default": {
    "status": 200
  },
  "rules": [],
  "checks": [],
  "vars": [],
  "schemas": [],
  "secrets": []
}
`
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("new endpoint's behaviour: %d\n%s", rec.Code, rec.Body)
	}
	if got, want := rec.Header().Get("ETag"), `"`+emulate.Version("")+`"`; got != want {
		t.Fatalf("ETag %q, want %q (the UI's version)", got, want)
	}
	wantError(t, e.do("GET", "/endpoints/7/behaviour", ""), http.StatusNotFound, "not_found", "")
	_ = ep
}

func TestPutBehaviour(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	_, tag := e.behaviour(t, ep.ID)
	body := `{"default":{"status":202,"body":"ok {{.N}}"},
		"rules":[{"when":{"kind":"every","n":3},"do":"reply","reply":{"status":503}}],
		"checks":[{"header":"X-Event","match":"present"}],
		"vars":[{"name":"TEAM","value":"qa"}]}`

	wantError(t, e.do("PUT", "/endpoints/1/behaviour", body), http.StatusPreconditionRequired, "version_required", "")
	wantError(t, e.putBehaviour(ep.ID, `"0000"`, body), http.StatusPreconditionFailed, "stale_version", "")
	wantError(t, e.putBehaviour(ep.ID, tag, `{"default":{"status":200,"colour":1}}`), http.StatusBadRequest, "unknown_field", "colour")

	rec := e.putBehaviour(ep.ID, tag, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	got := decodeAs[behaviourJSON](t, rec)
	cfg := e.stored(t, ep.ID)
	if got.Default.Status != 202 || len(cfg.Rules) != 1 || cfg.Rules[0].Reply.Status != 503 || len(cfg.Checks) != 1 || cfg.Vars[0].Value != "qa" {
		t.Fatalf("answer %+v, stored %+v", got, cfg)
	}
	newTag := rec.Header().Get("ETag")
	if newTag == tag || newTag != etag(cfg.String()) {
		t.Fatalf("ETag after a change %q (was %q)", newTag, tag)
	}
	// The ETag read before the change is stale now.
	wantError(t, e.putBehaviour(ep.ID, tag, body), http.StatusPreconditionFailed, "stale_version", "")
	// A weak form of the current one is accepted.
	if rec := e.putBehaviour(ep.ID, "W/"+newTag, `{}`); rec.Code != http.StatusOK || e.stored(t, ep.ID).Default.Status != 200 {
		t.Fatalf("empty behaviour: %d %s", rec.Code, rec.Body)
	}
}

func TestPutBehaviourInvalid(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	for body, field := range map[string]string{
		`{"rules":[{"when":{"kind":"every","n":1},"do":"reply","reply":{"status":200}},{"when":{"kind":"every","n":0},"do":"drop"}]}`: "rules[1]",
		`{"checks":[{"kind":"basic","secret":"NOPE","user":"u"}]}`:                                                                    "checks[0]",
		`{"default":{"status":99}}`:                                        "default",
		`{"vars":[{"name":"1x","value":""}]}`:                              "vars",
		`{"default":{"status":200,"delay_ms":3600000}}`:                    "default",
		`{"checks":[{"header":"X Bad","match":"present"}]}`:                "checks[0]",
		`{"rules":[{"when":{"kind":"sometimes"},"do":"reply"}]}`:           "rules[0]",
		`{"checks":[{"kind":"github","secret":"","enforce":true}]}`:        "checks[0]",
		`{"rules":[{"when":{"kind":"random","percent":101},"do":"drop"}]}`: "rules[0]",
	} {
		_, tag := e.behaviour(t, ep.ID)
		got := wantError(t, e.putBehaviour(ep.ID, tag, body), http.StatusUnprocessableEntity, "invalid", field)
		if strings.Contains(got.Error.Message, "{") {
			t.Fatalf("message %q", got.Error.Message)
		}
	}
	if cfg := e.stored(t, ep.ID); len(cfg.Rules)+len(cfg.Checks)+len(cfg.Vars) != 0 || cfg.Default.Status != 200 {
		t.Fatalf("an invalid behaviour was stored: %+v", cfg)
	}
}

func TestPutBehaviourKeepsSecrets(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	if rec := e.do("PUT", "/endpoints/1/secrets/PASS", `{"kind":"token","value":"s3cret-pass"}`); rec.Code != http.StatusOK {
		t.Fatalf("secret: %d %s", rec.Code, rec.Body)
	}
	b, tag := e.behaviour(t, ep.ID)
	// What was read, sent back with a check added: secrets are ignored, kept.
	b.Checks = append(b.Checks, emulate.Check{Kind: emulate.KindBasic, Secret: "PASS", User: "sender", Enforce: true})
	b.Secrets = nil
	rec := e.putBehaviour(ep.ID, tag, string(mustJSON(t, b)))
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	cfg := e.stored(t, ep.ID)
	if len(cfg.Secrets) != 1 || cfg.Secrets[0].Hash == "" || len(cfg.Checks) != 1 {
		t.Fatalf("stored %+v", cfg)
	}
	got := decodeAs[behaviourJSON](t, rec)
	if len(got.Secrets) != 1 || got.Secrets[0].UsedBy[0] != "check 1 (Basic credentials)" {
		t.Fatalf("secrets in the answer %+v", got.Secrets)
	}
}

func TestConcurrentPutsOneWins(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	_, tag := e.behaviour(t, ep.ID)
	codes := make(chan int, 20)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			codes <- e.putBehaviour(ep.ID, tag, fmt.Sprintf(`{"default":{"status":%d}}`, 200+i)).Code
		})
	}
	wg.Wait()
	close(codes)
	won := 0
	for c := range codes {
		switch c {
		case http.StatusOK:
			won++
		case http.StatusPreconditionFailed:
		default:
			t.Fatalf("status %d", c)
		}
	}
	if won != 1 {
		t.Fatalf("%d puts with the same ETag succeeded, want 1", won)
	}
}

// TestPutBehaviourChangedMeanwhile: a change saved by someone else after the
// If-Match check and before the save is not overwritten.
func TestPutBehaviourChangedMeanwhile(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	_, tag := e.behaviour(t, ep.ID)
	e.h.beforeSave = func() {
		if err := e.st.SetConfig(context.Background(), ep.ID, `{"default":{"status":418}}`); err != nil {
			t.Error(err)
		}
	}
	wantError(t, e.putBehaviour(ep.ID, tag, `{"default":{"status":202}}`), http.StatusPreconditionFailed, "stale_version", "")
	if got := e.stored(t, ep.ID).Default.Status; got != 418 {
		t.Fatalf("the other change was overwritten: status %d", got)
	}
}

func TestSchemasThroughTheAPI(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/orders")
	_, tag := e.behaviour(t, ep.ID)
	body := `{"schemas":[{"name":"OrderPaid","text":"{\"type\":\"object\",\"required\":[\"order_id\"]}"}],
		"checks":[{"kind":"schema","select":{"from":"body","key":"/type","map":[{"value":"order.paid","schema":"OrderPaid"}]},"enforce":true}]}`
	rec := e.putBehaviour(ep.ID, tag, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	got := decodeAs[behaviourJSON](t, rec)
	if len(got.Schemas) != 1 || got.Schemas[0].Name != "OrderPaid" || got.Checks[0].Select.Map[0].Schema != "OrderPaid" {
		t.Fatalf("answer %+v", got)
	}
	_, tag = e.behaviour(t, ep.ID)
	wantError(t, e.putBehaviour(ep.ID, tag, `{"schemas":[{"name":"R","text":"{\"$ref\":\"https://example.com/x.json\"}"}]}`),
		http.StatusUnprocessableEntity, "invalid", "schemas")
	wantError(t, e.putBehaviour(ep.ID, tag, `{"checks":[{"kind":"schema","schema":"Missing"}],"schemas":[{"name":"A","text":"{}"}]}`),
		http.StatusUnprocessableEntity, "invalid", "checks[0]")
}
