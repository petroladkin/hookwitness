package mcp

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// A request that creates no initialized session (a client probing a newer
// protocol before it initializes) gives its slot back before it is
// answered, so the client's initialize right after is never refused.
func TestSessionLimitProbeFreesAtOnce(t *testing.T) {
	srv, h := limitServer(t, 1, io.Discard)
	for range 20 {
		req, _ := http.NewRequest("POST", srv.URL+"/admin/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if n := h.slotsInUse(); n != 0 {
			t.Fatalf("%d slots held after a probe was answered", n)
		}
	}
	if s, _, _, _ := initialize(t, srv, 1); s != http.StatusOK {
		t.Fatalf("initialize after probes: %d", s)
	}
}
