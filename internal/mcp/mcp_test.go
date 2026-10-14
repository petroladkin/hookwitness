package mcp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/petroladkin/hookwitness/internal/api"
	"github.com/petroladkin/hookwitness/internal/catch"
	"github.com/petroladkin/hookwitness/internal/live"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type env struct {
	session *sdk.ClientSession
	catcher *catch.Handler
	logs    *syncBuffer
	results *syncBuffer // every tool result's text, for the leak test
}

// newEnv is the REST API and the catcher on one store, and an MCP client
// session with this package's server over in-memory transports.
func newEnv(t *testing.T) env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	keys, err := secret.Load("", filepath.Join(t.TempDir(), "hw.db.key"))
	if err != nil {
		t.Fatal(err)
	}
	lv := live.New()
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	a := api.New(st, lv, log, api.Options{Prefix: "/admin", MaxWait: time.Minute, Keys: keys, Keyring: keys, Stop: stop, Version: "v0.0.0-test"})
	catcher := &catch.Handler{Store: st, Live: lv, Log: log, MaxBody: 1 << 20, Retention: 1000, Keys: keys}

	srv := NewServer(a, api.Base("/admin"), "127.0.0.1:8080", "v0.0.0-test")
	ct, stt := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), stt, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cl := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := cl.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return env{cs, catcher, logs, &syncBuffer{}}
}

// call calls a tool; it fails the test on a protocol error, not on a tool
// error (IsError), which tests check themselves.
func (e env) call(t *testing.T, ctx context.Context, name string, args map[string]any) (*sdk.CallToolResult, string) {
	t.Helper()
	res, err := e.session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	e.results.Write([]byte(text.String() + "\n"))
	return res, text.String()
}

// ok calls a tool that must succeed and returns its structured result.
func (e env) ok(t *testing.T, name string, args map[string]any) (map[string]any, string) {
	t.Helper()
	res, text := e.call(t, context.Background(), name, args)
	if res.IsError {
		t.Fatalf("%s: %s", name, text)
	}
	out := map[string]any{}
	b, _ := json.Marshal(res.StructuredContent)
	json.Unmarshal(b, &out)
	return out, text
}

// deliver sends a webhook to the catcher, as a sender would.
func (e env) deliver(path, body string, hdr ...string) int {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.catcher.Catch(rec, r, nil, false)
	return rec.Code
}

func githubSig(key, body string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// TestToolList: the tools of the plan, each with a description, an object
// input schema and the annotations an agent's client asks the user by.
func TestToolList(t *testing.T) {
	e := newEnv(t)
	list, err := e.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*sdk.Tool{}
	for _, tool := range list.Tools {
		got[tool.Name] = tool
	}
	want := []string{"create_endpoint", "list_endpoints", "get_endpoint", "configure_endpoint", "set_secret", "wait_for_request",
		"list_requests", "get_request", "expect", "reset_endpoint", "recheck", "delete_endpoint"}
	if len(got) != len(want) {
		t.Fatalf("%d tools, want %d", len(got), len(want))
	}
	for _, name := range want {
		tool := got[name]
		if tool == nil || len(tool.Description) < 30 || tool.InputSchema == nil || tool.Annotations == nil {
			t.Fatalf("%s: missing, or without description, schema or annotations", name)
		}
	}
	for _, name := range []string{"list_endpoints", "get_endpoint", "wait_for_request", "list_requests", "get_request", "expect"} {
		if !got[name].Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", name)
		}
	}
	for _, name := range []string{"delete_endpoint", "reset_endpoint"} {
		if a := got[name].Annotations; a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint {
			t.Errorf("%s is not marked destructive", name)
		}
	}
	for _, name := range []string{"create_endpoint", "configure_endpoint", "set_secret", "recheck"} {
		if a := got[name].Annotations; a.DestructiveHint == nil || *a.DestructiveHint {
			t.Errorf("%s is marked destructive", name)
		}
	}
}

// TestSignatureScenario: the agent's story end to end: an endpoint that
// demands a GitHub signature, one good and one bad delivery, and expect
// gives pass for one and fail with the reason for two.
func TestSignatureScenario(t *testing.T) {
	e := newEnv(t)
	ep, text := e.ok(t, "create_endpoint", map[string]any{"path": "/github/events"})
	if ep["url"] != "http://127.0.0.1:8080/github/events" || !strings.Contains(text, "send webhooks to http://127.0.0.1:8080/github/events") {
		t.Fatalf("create: %s", text)
	}
	if _, text := e.ok(t, "set_secret", map[string]any{"endpoint": "/github/events", "name": "GH", "kind": "key", "value": "gh_test_secret"}); !strings.Contains(text, "fingerprint") {
		t.Fatalf("secret: %s", text)
	}
	e.ok(t, "configure_endpoint", map[string]any{"endpoint": "/github/events", "checks": []any{map[string]any{"kind": "github", "secret": "GH"}}})

	body := `{"action":"opened"}`
	go func() {
		time.Sleep(150 * time.Millisecond)
		e.deliver("/github/events", body, "X-Hub-Signature-256", githubSig("gh_test_secret", body))
		e.deliver("/github/events", body, "X-Hub-Signature-256", githubSig("wrong", body))
	}()
	out, text := e.ok(t, "expect", map[string]any{"endpoint": "/github/events", "verdict": "pass", "within_s": 10})
	if out["result"] != "pass" || !strings.HasPrefix(text, "pass: 1 request(s) with verdict=pass arrived") || out["summary"] != strings.SplitN(text, "\n", 2)[0] {
		t.Fatalf("expect pass: %s", text)
	}
	time.Sleep(100 * time.Millisecond) // the bad one is caught too
	out, text = e.ok(t, "expect", map[string]any{"endpoint": "/github/events", "verdict": "pass", "count": 2, "after": 0, "within_s": 1})
	if out["result"] != "fail" || !strings.Contains(text, "2 arrived, 1 matched") || !strings.Contains(text, "GitHub signature: signature does not match") {
		t.Fatalf("expect fail: %s", text)
	}
	got, text := e.ok(t, "get_request", map[string]any{"id": 2})
	if got["verdict"] != "fail" || !strings.HasPrefix(text, "#2 POST /github/events answered 200, verdict fail (GitHub signature: signature does not match)") {
		t.Fatalf("get_request: %s", text)
	}
}

// TestWait: a request arriving during the wait ends it at once; the time
// running out is reported, not an error; cancelling the call ends it.
func TestWait(t *testing.T) {
	e := newEnv(t)
	e.ok(t, "create_endpoint", map[string]any{"path": "/w"})
	e.deliver("/w", `{"old":1}`) // before the call: not "the next one"
	go func() { time.Sleep(200 * time.Millisecond); e.deliver("/w", `{"n":1}`) }()
	start := time.Now()
	got, text := e.ok(t, "wait_for_request", map[string]any{"endpoint": "/w", "timeout_s": 20})
	if got["arrived"] != true || got["body_text"] != `{"n":1}` || time.Since(start) > 10*time.Second {
		t.Fatalf("wait: %s after %v", text, time.Since(start))
	}
	start = time.Now()
	got, text = e.ok(t, "wait_for_request", map[string]any{"endpoint": "/w", "timeout_s": 1, "text": "never"})
	if got["arrived"] != false || !strings.HasPrefix(text, "No matching request within 1s") || time.Since(start) < time.Second {
		t.Fatalf("timeout: %s after %v (never before the time is up)", text, time.Since(start))
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.session.CallTool(ctx, &sdk.CallToolParams{Name: "wait_for_request", Arguments: map[string]any{"endpoint": "/w", "timeout_s": 300}})
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling the call did not end the wait")
	}
}

// TestConfigureMergesParts: the parts given replace those parts, the
// others stay; the API's validation errors come back as tool errors.
func TestConfigureMergesParts(t *testing.T) {
	e := newEnv(t)
	e.ok(t, "create_endpoint", map[string]any{"path": "/m"})
	e.ok(t, "configure_endpoint", map[string]any{"endpoint": "/m", "checks": []any{map[string]any{"header": "X-Key", "match": "present"}}})
	e.ok(t, "configure_endpoint", map[string]any{"endpoint": "/m", "rules": []any{map[string]any{"when": map[string]any{"kind": "every", "n": 3}, "do": "drop"}}})
	ep, _ := e.ok(t, "get_endpoint", map[string]any{"endpoint": "/m"})
	b := ep["behaviour"].(map[string]any)
	if len(b["checks"].([]any)) != 1 || len(b["rules"].([]any)) != 1 {
		t.Fatalf("behaviour %v", b)
	}
	res, text := e.call(t, context.Background(), "configure_endpoint", map[string]any{"endpoint": "/m", "default": map[string]any{"status": 99}})
	if !res.IsError || !strings.Contains(text, "invalid") || !strings.Contains(text, "status 99") {
		t.Fatalf("an invalid default: %v %s", res.IsError, text)
	}
	res, text = e.call(t, context.Background(), "get_endpoint", map[string]any{"endpoint": "/nope"})
	if !res.IsError || !strings.Contains(text, "no endpoint with the path /nope") {
		t.Fatalf("an unknown endpoint: %s", text)
	}
}

// TestNoSecretInResults: a secret's value never comes back in a tool
// result or the log, whatever the agent asks.
func TestNoSecretInResults(t *testing.T) {
	e := newEnv(t)
	const value = "tok-leak-probe-5e9f"
	e.ok(t, "create_endpoint", map[string]any{"path": "/s"})
	e.ok(t, "set_secret", map[string]any{"endpoint": "/s", "name": "T", "kind": "token", "value": value})
	e.ok(t, "configure_endpoint", map[string]any{"endpoint": "/s", "checks": []any{map[string]any{"kind": "bearer", "secret": "T"}}})
	e.deliver("/s", "{}")
	e.ok(t, "get_endpoint", map[string]any{"endpoint": "/s"})
	e.ok(t, "list_endpoints", map[string]any{})
	e.ok(t, "list_requests", map[string]any{"endpoint": "/s", "include_body": true})
	e.ok(t, "get_request", map[string]any{"id": 1})
	e.ok(t, "expect", map[string]any{"endpoint": "/s", "after": 0, "within_s": 1})
	e.ok(t, "recheck", map[string]any{"endpoint": "/s"})
	if strings.Contains(e.results.String(), value) || strings.Contains(e.logs.String(), value) {
		t.Fatal("the secret's value is in a result or the log")
	}
}

// TestBodyCut: a body over 16 KiB is cut, with its full size and where the
// whole body is.
func TestBodyCut(t *testing.T) {
	e := newEnv(t)
	e.ok(t, "create_endpoint", map[string]any{"path": "/big"})
	e.deliver("/big", `{"s":"`+strings.Repeat("é", 12000)+`"}`)
	got, _ := e.ok(t, "get_request", map[string]any{"id": 1})
	if s := got["body_text"].(string); len(s) > MaxBody || !strings.HasPrefix(s, `{"s":"é`) || got["body_cut"] != true ||
		got["body_url"] != "http://127.0.0.1:8080/admin/api/v1/requests/1/body" || got["body_size"].(float64) < 24000 {
		t.Fatalf("cut body: %d bytes, cut %v, url %v, size %v", len(s), got["body_cut"], got["body_url"], got["body_size"])
	}
}

// TestResetAndDelete: the destructive tools do what they say.
func TestResetAndDelete(t *testing.T) {
	e := newEnv(t)
	e.ok(t, "create_endpoint", map[string]any{"path": "/r"})
	e.deliver("/r", "{}")
	if res, text := e.call(t, context.Background(), "reset_endpoint", map[string]any{"endpoint": "/r"}); !res.IsError || !strings.Contains(text, "say what to reset") {
		t.Fatalf("nothing to reset: %s", text)
	}
	e.ok(t, "reset_endpoint", map[string]any{"endpoint": "/r", "requests": true, "counter": true})
	if out, _ := e.ok(t, "list_requests", map[string]any{"endpoint": "/r"}); len(out["requests"].([]any)) != 0 {
		t.Fatal("requests not cleared")
	}
	e.ok(t, "delete_endpoint", map[string]any{"endpoint": "/r"})
	out, _ := e.ok(t, "list_endpoints", map[string]any{})
	if eps, _ := out["endpoints"].([]any); slices.ContainsFunc(eps, func(x any) bool { return x.(map[string]any)["path"] == "/r" }) {
		t.Fatal("endpoint not deleted")
	}
}
