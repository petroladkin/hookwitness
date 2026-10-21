package mcp

import (
	"context"
	"strings"
	"testing"
)

func TestSwitchEndpoint(t *testing.T) {
	e := newEnv(t)
	e.ok(t, "create_endpoint", map[string]any{"path": "/s"})
	e.ok(t, "create_endpoint", map[string]any{"path": "/t"})
	if res, text := e.call(t, context.Background(), "switch_endpoint", map[string]any{"endpoint": "/s"}); !res.IsError {
		t.Fatalf("without enabled: %s", text)
	}
	out, text := e.ok(t, "switch_endpoint", map[string]any{"endpoint": "/s", "enabled": false})
	if out["enabled"] != false || !strings.Contains(text, "/s is off") || !strings.Contains(text, "Unmatched") {
		t.Fatalf("turn off: %s", text)
	}
	if code := e.deliver("/s", "{}"); code != 404 {
		t.Fatalf("delivery to the off endpoint: %d", code)
	}
	if _, text := e.ok(t, "list_endpoints", map[string]any{}); !strings.HasPrefix(text, "2 endpoint(s), 1 off") {
		t.Fatalf("list: %s", text)
	}
	if _, text := e.ok(t, "get_endpoint", map[string]any{"endpoint": "/s"}); !strings.Contains(text, "off") {
		t.Fatalf("get: %s", text)
	}
	out, text = e.ok(t, "switch_endpoint", map[string]any{"endpoint": "/s", "enabled": true})
	if out["enabled"] != true || !strings.Contains(text, "/s is on") {
		t.Fatalf("turn on: %s", text)
	}
	if code := e.deliver("/s", "{}"); code != 200 {
		t.Fatalf("delivery after turning on: %d", code)
	}
}
