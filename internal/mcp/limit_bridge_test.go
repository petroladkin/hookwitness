package mcp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A stdio client bridged to a server whose session limit is reached learns
// why: the bridge ends with the limit named.
func TestBridgeOverTheSessionLimit(t *testing.T) {
	srv, _ := limitServer(t, 1, io.Discard)
	if s, _, _, _ := initialize(t, srv, 1); s != http.StatusOK {
		t.Fatalf("first session: %d", s)
	}
	in, w := io.Pipe()
	go io.WriteString(w, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`+"\n")
	var out strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Bridge(ctx, srv.URL+"/admin/mcp", srv.Client(), in, nopWriteCloser{&out})
	t.Logf("bridge: %v; wrote %q", err, out.String())
	if err == nil || !strings.Contains(err.Error(), "session limit") {
		t.Fatalf("err = %v, want the session limit named", err)
	}
}
