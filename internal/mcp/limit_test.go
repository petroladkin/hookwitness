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
	h := Handler(a, api.Base("/admin"), "test", log, nil, limit)
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
