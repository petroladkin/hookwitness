package mcp

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/petroladkin/hookwitness/internal/store"
)

// configure_endpoint takes a receiver, says that live traffic leaves the
// server and warns of an open relay; requests then read "→ receiver 200";
// false removes it.
func TestForwardTools(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 4096)
				c.Read(buf)
				io.WriteString(c, "HTTP/1.1 201 Created\r\nContent-Length: 2\r\n\r\nok")
			}()
		}
	}()
	e := newEnv(t)
	e.ok(t, "create_endpoint", map[string]any{"path": "/live"})
	_, text := e.ok(t, "configure_endpoint", map[string]any{"endpoint": "/live", "forward": map[string]any{"url": "http://" + ln.Addr().String() + "/in"}})
	if !strings.Contains(text, "forwarded to http://"+ln.Addr().String()+"/in (live traffic leaves the server)") || !strings.Contains(text, "warning: anyone who reaches") {
		t.Fatalf("configure: %s", text)
	}
	if code := e.deliver("/live", `{"a":1}`); code != 201 {
		t.Fatalf("delivered: %d", code)
	}
	if _, text := e.ok(t, "get_request", map[string]any{"id": 1}); !strings.Contains(text, "#1 POST /live → receiver 201 in") {
		t.Fatalf("get_request: %s", text)
	}
	if res, text := e.call(t, context.Background(), "configure_endpoint", map[string]any{"endpoint": "/live", "forward": map[string]any{"url": "http://169.254.169.254/"}}); !res.IsError ||
		!strings.Contains(text, "link-local") {
		t.Fatalf("a refused target: %s", text)
	}
	if out, text := e.ok(t, "configure_endpoint", map[string]any{"endpoint": "/live", "forward": false}); out["forward"] != nil || strings.Contains(text, "forwarded to") {
		t.Fatalf("removed: %s", text)
	}
	if code := e.deliver("/live", `{"a":2}`); code != 200 {
		t.Fatalf("after removing: %d", code)
	}
}

// AC-8 through MCP: a user's token cannot add a receiver; an admin's can.
func TestForwardToolNeedsAdmin(t *testing.T) {
	ts := newTokenServer(t)
	bob, err := ts.st.AddUser(context.Background(), "bob", store.RoleUser, ts.auth.HashPassword("correct horse battery"))
	if err != nil {
		t.Fatal(err)
	}
	connect := func(u store.User) *sdk.ClientSession {
		v, _, err := ts.auth.MintFor(context.Background(), u.ID, "agent")
		if err != nil {
			t.Fatal(err)
		}
		cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(),
			&sdk.StreamableClientTransport{Endpoint: ts.srv.URL + "/admin/mcp", HTTPClient: &http.Client{Transport: bearer{v}}, MaxRetries: -1}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cs.Close() })
		return cs
	}
	text := func(res *sdk.CallToolResult) string {
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*sdk.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return b.String()
	}
	user, admin := connect(bob), connect(ts.user)
	if _, err := admin.CallTool(context.Background(), &sdk.CallToolParams{Name: "create_endpoint", Arguments: map[string]any{"path": "/live"}}); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"endpoint": "/live", "forward": map[string]any{"url": "https://receiver.test/in"}}
	res, err := user.CallTool(context.Background(), &sdk.CallToolParams{Name: "configure_endpoint", Arguments: args})
	if err != nil || !res.IsError || !strings.Contains(text(res), "only an admin can change where requests are forwarded") {
		t.Fatalf("a user: %v %s", err, text(res))
	}
	if _, cfg, _, _ := ts.st.EndpointByPath(context.Background(), "/live"); strings.Contains(cfg, "forward") {
		t.Fatalf("stored after a refusal: %s", cfg)
	}
	if res, err := admin.CallTool(context.Background(), &sdk.CallToolParams{Name: "configure_endpoint", Arguments: args}); err != nil || res.IsError {
		t.Fatalf("an admin: %v %s", err, text(res))
	}
}
