package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
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
