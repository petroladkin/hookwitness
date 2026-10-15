package web

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/petroladkin/hookwitness/internal/server"
)

// TestMCPOverHTTP: an MCP client reaches the tools at <prefix>/mcp over
// Streamable HTTP; the catch URL it gets is the host it called. The
// endpoint has the API's access rules: an unknown host name and a request
// from another site are refused.
func TestMCPOverHTTP(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	cl := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := cl.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: srv.URL + "/admin/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if cs.InitializeResult().ServerInfo.Name != "hookwitness" {
		t.Fatalf("server %+v", cs.InitializeResult().ServerInfo)
	}
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "create_endpoint", Arguments: map[string]any{"path": "/over/http"}})
	if err != nil || res.IsError {
		t.Fatalf("%v %v", err, res)
	}
	text := res.Content[0].(*sdk.TextContent).Text
	if !strings.Contains(text, strings.TrimPrefix(srv.URL, "http://")+"/over/http") {
		t.Fatalf("catch URL not on the host called: %s", text)
	}

	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`
	post := func(host, origin string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/admin/mcp", strings.NewReader(init))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if host != "" {
			req.Host = host
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
			req.Header.Set("Sec-Fetch-Site", "cross-site")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("hooks.example.com", ""); code != http.StatusMisdirectedRequest {
		t.Fatalf("unknown host: %d, want 421", code)
	}
	if code := post("", "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("cross-origin: %d, want 403", code)
	}
	if code := post("", ""); code != http.StatusOK {
		t.Fatalf("plain initialize: %d", code)
	}
}

// A graceful stop with an MCP client attached (its event stream open, a tool
// waiting five minutes) ends at once and without an error: the wait ends,
// the sessions close, and the HTTP server has nothing left to wait for.
func TestShutdownWithAnMCPClient(t *testing.T) {
	e := newEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var inflight atomic.Int64
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)
		e.h.ServeHTTP(w, r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- server.Serve(ctx, ln, counted, slog.New(slog.NewTextHandler(io.Discard, nil)), e.h.StopStreams)
	}()
	cl := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := cl.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: "http://" + ln.Addr().String() + "/admin/mcp", MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "create_endpoint", Arguments: map[string]any{"path": "/s"}}); err != nil || res.IsError {
		t.Fatalf("%v %v", err, res)
	}
	called := make(chan struct{})
	go func() {
		defer close(called)
		cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "wait_for_request", Arguments: map[string]any{"endpoint": "/s", "timeout_s": 300}})
	}()
	for deadline := time.Now().Add(10 * time.Second); inflight.Load() < 2; { // the event stream and the waiting call
		if time.Now().After(deadline) {
			t.Fatalf("%d requests in flight, want the stream and the call", inflight.Load())
		}
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	cancel()
	if err := <-served; err != nil {
		t.Fatalf("the server stopped with %v", err)
	}
	if took := time.Since(start); took >= server.ShutdownTimeout/2 {
		t.Fatalf("the stop took %v: it waited for the MCP session", took)
	}
	<-called
}

// A session the client closes is forgotten: the list of open sessions does
// not grow with every client that came and went.
func TestMCPSessionsForgotten(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	cl := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := cl.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: srv.URL + "/admin/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); e.h.mcp.Sessions() != 1; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions open, want 1", e.h.mcp.Sessions())
		}
	}
	cs.Close()
	for deadline := time.Now().Add(10 * time.Second); e.h.mcp.Sessions() != 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions open after the client closed", e.h.mcp.Sessions())
		}
	}
}
