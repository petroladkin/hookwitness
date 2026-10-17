package mcp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/petroladkin/hookwitness/internal/api"
	"github.com/petroladkin/hookwitness/internal/live"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
)

func TestEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:8080":            "http://127.0.0.1:8080/admin/mcp",
		"http://127.0.0.1:8080/":           "http://127.0.0.1:8080/admin/mcp",
		"https://hooks.example.com/ui":     "https://hooks.example.com/ui/mcp",
		"http://localhost:8080/admin/mcp/": "http://localhost:8080/admin/mcp",
	} {
		if got, err := Endpoint(in); err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "127.0.0.1:8080", "ftp://x"} {
		if _, err := Endpoint(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestBridge: a client on stdio reaches the server's tools through the
// bridge, message for message, and the bridge ends when stdin closes.
func TestBridge(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	keys, _ := secret.Load("", filepath.Join(t.TempDir(), "hw.db.key"))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := api.New(st, live.New(), log, api.Options{Prefix: "/admin", MaxWait: time.Minute, Keys: keys, Keyring: keys, Stop: make(chan struct{}), Version: "test"})
	mux := http.NewServeMux()
	mux.Handle("/admin/mcp", Handler(a, api.Base("/admin"), "test", log, nil))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	toBridge, clientOut := io.Pipe()
	clientIn, fromBridge := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Bridge(context.Background(), srv.URL+"/admin/mcp", srv.Client(), toBridge, fromBridge) }()

	cl := sdk.NewClient(&sdk.Implementation{Name: "stdio-client", Version: "1"}, nil)
	cs, err := cl.Connect(context.Background(), &sdk.IOTransport{Reader: clientIn, Writer: clientOut}, nil)
	if err != nil {
		t.Fatal(err)
	}
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(list.Tools) != 12 {
		t.Fatalf("tools through the bridge: %v %v", err, list)
	}
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "create_endpoint", Arguments: map[string]any{"path": "/via/stdio"}})
	if err != nil || res.IsError || !strings.Contains(res.Content[0].(*sdk.TextContent).Text, "/via/stdio") {
		t.Fatalf("call through the bridge: %v %v", err, res)
	}
	cs.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("bridge ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the bridge did not end when the client closed")
	}
}
