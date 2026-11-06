package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/petroladkin/hookwitness/internal/api"
	"github.com/petroladkin/hookwitness/internal/live"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
)

// limitServer is an MCP endpoint at /admin/mcp allowing limit sessions.
func limitServer(t *testing.T, limit int, logs io.Writer) (*httptest.Server, *Streamable) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	keys, _ := secret.Load("", filepath.Join(t.TempDir(), "hw.db.key"))
	log := slog.New(slog.NewTextHandler(logs, nil))
	a := api.New(st, live.New(), log, api.Options{Prefix: "/admin", Reserved: "/admin", MaxWait: time.Minute, Keys: keys, Keyring: keys, Stop: make(chan struct{}), Version: "test"})
	h := Handler(a, api.Base("/admin"), "test", log, nil, limit, nil)
	mux := http.NewServeMux()
	mux.Handle("/admin/mcp", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { h.CloseSessions(); srv.Close() })
	return srv, h
}

func connect(srv *httptest.Server) (*sdk.ClientSession, error) {
	cl := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	return cl.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: srv.URL + "/admin/mcp", MaxRetries: -1}, nil)
}

// initialize sends a bare initialize, as a client opening a session does,
// and returns the answer's status, Retry-After, session id and body.
func initialize(t *testing.T, srv *httptest.Server, id int) (int, string, string, string) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"1"}}}`
	req, _ := http.NewRequest("POST", srv.URL+"/admin/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header.Get("Retry-After"), res.Header.Get("Mcp-Session-Id"), string(b)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func (s *Streamable) slotsInUse() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.slots
}

// waitFor polls cond for up to 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
	}
}

// AC-1, AC-2: with the limit at 2 the third session is refused at
// initialize with 503, Retry-After and a JSON-RPC error naming the limit,
// the open sessions keep working, and a closed one frees its slot.
func TestSessionLimit(t *testing.T) {
	var logs syncBuffer
	srv, h := limitServer(t, 2, &logs)
	a, err := connect(srv)
	if err != nil {
		t.Fatal(err)
	}
	b, err := connect(srv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connect(srv); err == nil {
		t.Fatal("a third session was opened with the limit at 2")
	}
	status, retry, sid, body := initialize(t, srv, 7)
	if status != http.StatusServiceUnavailable || retry != "30" || sid != "" {
		t.Fatalf("over the limit: %d Retry-After %q session %q", status, retry, sid)
	}
	var rpc struct {
		ID    int `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &rpc); err != nil || rpc.ID != 7 || rpc.Error.Code != -32000 ||
		!strings.Contains(rpc.Error.Message, "too many MCP sessions (limit 2)") {
		t.Fatalf("the refusal: %v %s", err, body)
	}
	if n := strings.Count(logs.String(), "session refused"); n != 1 {
		t.Fatalf("refusals logged %d times, want once a minute:\n%s", n, logs.String())
	}

	// The open sessions are unaffected.
	for _, cs := range []*sdk.ClientSession{a, b} {
		if _, err := cs.ListTools(context.Background(), nil); err != nil {
			t.Fatalf("an open session was refused: %v", err)
		}
	}

	// A session closed by its client gives its slot back.
	a.Close()
	waitFor(t, "a free slot", func() bool { s, _, _, _ := initialize(t, srv, 8); return s == http.StatusOK })
	if h.slotsInUse() != 2 {
		t.Fatalf("%d slots in use, want 2 (b and the raw session)", h.slotsInUse())
	}
	b.Close()
}

// AC-2: an idle session ended by the timeout frees its slot too.
func TestSessionLimitIdleExpiry(t *testing.T) {
	old := sessionTimeout
	sessionTimeout = 300 * time.Millisecond
	t.Cleanup(func() { sessionTimeout = old })
	srv, h := limitServer(t, 1, io.Discard)
	if s, _, sid, _ := initialize(t, srv, 1); s != http.StatusOK || sid == "" {
		t.Fatalf("first session: %d %q", s, sid)
	}
	if s, _, _, _ := initialize(t, srv, 2); s != http.StatusServiceUnavailable {
		t.Fatalf("second while the first is open: %d", s)
	}
	waitFor(t, "the idle session to expire", func() bool { return h.slotsInUse() == 0 })
	if s, _, _, _ := initialize(t, srv, 3); s != http.StatusOK {
		t.Fatalf("after the expiry: %d", s)
	}
}

// AC-2: a burst of initializes never opens more sessions than the limit.
func TestSessionLimitBurst(t *testing.T) {
	srv, h := limitServer(t, 3, io.Discard)
	var mu sync.Mutex
	codes := map[int]int{}
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, _, _, _ := initialize(t, srv, i+1)
			mu.Lock()
			codes[s]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[http.StatusOK] != 3 || codes[http.StatusServiceUnavailable] != 7 {
		t.Fatalf("answers %v, want 3 × 200 and 7 × 503", codes)
	}
	if h.slotsInUse() != 3 {
		t.Fatalf("%d slots in use", h.slotsInUse())
	}
}

// AC-2: 0 means no limit; a failed initialize holds no slot.
func TestSessionLimitOff(t *testing.T) {
	srv, h := limitServer(t, 0, io.Discard)
	for i := range 40 {
		if s, _, _, _ := initialize(t, srv, i+1); s != http.StatusOK {
			t.Fatalf("session %d: %d", i+1, s)
		}
	}
	req, _ := http.NewRequest("POST", srv.URL+"/admin/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	waitFor(t, "40 slots", func() bool { return h.slotsInUse() == 40 })
}

// bigSetup is a setup file of n endpoints, each with a schema of size bytes.
func bigSetup(n, size int) map[string]any {
	eps := []any{}
	for i := range n {
		eps = append(eps, map[string]any{"path": "/big/" + itoa(i), "default": map[string]any{"status": 200},
			"schemas": []any{map[string]any{"name": "Big", "text": `{"type":"object","description":"` + strings.Repeat("x", size) + `"}`}}})
	}
	return map[string]any{"hookwitness": 1, "endpoints": eps}
}

func toolText(res *sdk.CallToolResult) string {
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// Over HTTP, import_endpoints takes any setup the API takes (16 MiB): one
// of 10 MiB is imported; one the API finds too large is a tool error; a
// request past MaxRequest is refused by the transport (413, naming the
// limit), and the server keeps the session. export_endpoint gives at most
// 4 MiB, and above that says how to get the file.
func TestSetupSizesOverHTTP(t *testing.T) {
	srv, h := limitServer(t, 0, io.Discard)
	cs, err := connect(srv)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	ctx := context.Background()
	call := func(name string, args map[string]any) (*sdk.CallToolResult, error) {
		return cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	}
	res, err := call("import_endpoints", map[string]any{"setup": bigSetup(10, 1<<20)})
	if err != nil || res.IsError {
		t.Fatalf("a 10 MiB setup: %v %s", err, toolText(res))
	}
	res, err = call("import_endpoints", map[string]any{"setup": bigSetup(11, 3<<19)}) // 16.5 MiB
	if err != nil || !res.IsError || !strings.Contains(toolText(res), "the setup file is over 16777216 bytes") {
		t.Fatalf("a 16.5 MiB setup: %v %+v", err, res)
	}

	res, err = call("export_endpoint", map[string]any{"endpoints": []any{"/big/3"}})
	if err != nil || res.IsError || len(toolText(res)) > MaxExport {
		t.Fatalf("one endpoint's export: %v, %d bytes", err, len(toolText(res)))
	}
	res, err = call("export_endpoint", map[string]any{})
	if err != nil || !res.IsError {
		t.Fatalf("a 10 MiB export: %v, error %v", err, res.IsError)
	}
	if text := toolText(res); !strings.Contains(text, "the setup is 10.0 MiB, over the 4 MiB a result carries") || !strings.Contains(text, "GET /admin/api/v1/setup") || len(text) > 1000 {
		t.Fatalf("a 10 MiB export: %s", text)
	}

	// 18 MiB: the transport refuses it. This client then ends its session
	// itself; the server keeps a session whose request was too large.
	if _, err = call("import_endpoints", map[string]any{"setup": bigSetup(12, 3<<19)}); err == nil || !strings.Contains(err.Error(), "Request Entity Too Large") {
		t.Fatalf("an 18 MiB request: %v", err)
	}
	_, _, sid, _ := initialize(t, srv, 1)
	waitFor(t, "one session", func() bool { return h.Sessions() == 1 })
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 99, "method": "tools/call",
		"params": map[string]any{"name": "import_endpoints", "arguments": map[string]any{"setup": bigSetup(12, 3<<19)}}})
	req, _ := http.NewRequest("POST", srv.URL+"/admin/mcp", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", sid)
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	r, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(r.Body); r.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(string(b), "request body exceeds 17825792 bytes") {
		t.Fatalf("an 18 MiB request: %d %s", r.StatusCode, b)
	}
	if h.Sessions() != 1 {
		t.Fatalf("the server dropped the session: %d", h.Sessions())
	}
}

// A session whose client sent initialize but never initialized is closed at
// shutdown like any other: its open stream ends, its slot comes back.
func TestCloseSessionNeverInitialized(t *testing.T) {
	srv, h := limitServer(t, 4, io.Discard)
	code, _, sid, body := initialize(t, srv, 1)
	if code != http.StatusOK || sid == "" {
		t.Fatalf("initialize: %d %q %s", code, sid, body)
	}
	if h.Sessions() != 1 || h.slotsInUse() != 1 {
		t.Fatalf("after initialize: %d sessions, %d slots", h.Sessions(), h.slotsInUse())
	}
	req, _ := http.NewRequest("GET", srv.URL+"/admin/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Mcp-Session-Id", sid)
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the GET stream: %d", res.StatusCode)
	}
	ended := make(chan struct{})
	go func() { io.Copy(io.Discard, res.Body); res.Body.Close(); close(ended) }()
	select {
	case <-ended:
		t.Fatal("the stream ended before the shutdown")
	case <-time.After(100 * time.Millisecond):
	}
	h.CloseSessions()
	select {
	case <-ended:
	case <-time.After(10 * time.Second): // the alternative is the 30-minute session timeout
		t.Fatal("the stream is still open after CloseSessions")
	}
	waitFor(t, "sessions and slots back to 0", func() bool { return h.Sessions() == 0 && h.slotsInUse() == 0 })
}
