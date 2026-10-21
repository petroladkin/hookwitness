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

// Only an initialize (or a body that cannot be told apart from one) takes a
// slot: an SDK client probes server/discover first and sends initialize as
// soon as the probe's answer arrives.
func TestOpens(t *testing.T) {
	for _, c := range []struct {
		body string
		want bool
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, true},
		{`{"jsonrpc":"2.0","id":1,"method":"server/discover"}`, false},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, false},
		{`[{"jsonrpc":"2.0","id":1,"method":"initialize"}]`, true},
		{`{"jsonrpc":"2.0","method":`, true},
		{`{"method":"tools/list","pad":"` + strings.Repeat("x", peekSize) + `"}`, true},
	} {
		if got := opens([]byte(c.body)); got != c.want {
			t.Errorf("opens(%.60s) = %v, want %v", c.body, got, c.want)
		}
	}
}
