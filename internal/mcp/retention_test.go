package mcp

import (
	"context"
	"strings"
	"testing"
)

func TestRetentionTools(t *testing.T) {
	e := newEnv(t)
	e.ok(t, "create_endpoint", map[string]any{"path": "/r"})
	for range 6 {
		e.deliver("/r", "{}")
	}
	list, _ := e.ok(t, "list_requests", map[string]any{"endpoint": "/r"})
	reqs, _ := list["requests"].([]any)
	first, _ := reqs[len(reqs)-1].(map[string]any)
	id := int64(num(first["id"]))

	out, text := e.ok(t, "pin_request", map[string]any{"request_id": id})
	if out["pinned"] != true || !strings.Contains(text, "pinned") {
		t.Fatalf("pin: %s", text)
	}
	out, text = e.ok(t, "set_retention", map[string]any{"endpoint": "/r", "normal": map[string]any{"count": 2, "max_age": "7d"}})
	if num(out["to_prune"]) != 3 || !strings.Contains(text, "the newest 2 normal requests for at most 7d") || !strings.Contains(text, "3 request(s) removed") {
		t.Fatalf("set_retention: %s", text)
	}
	if _, text := e.ok(t, "list_requests", map[string]any{"endpoint": "/r", "pinned": true}); !strings.Contains(text, "1 ") {
		t.Fatalf("the pinned filter: %s", text)
	}
	e.ok(t, "get_request", map[string]any{"id": id}) // the pinned request is still there
	out, text = e.ok(t, "set_retention", map[string]any{"endpoint": "/r", "defaults": true})
	if out["retention"] != nil || !strings.Contains(text, "the newest 1000 normal requests and the newest 1000 anomalies") {
		t.Fatalf("defaults: %s", text)
	}
	if res, text := e.call(t, context.Background(), "set_retention", map[string]any{"endpoint": "/r"}); !res.IsError {
		t.Fatalf("nothing given: %s", text)
	}
	if res, text := e.call(t, context.Background(), "set_retention", map[string]any{"endpoint": "unmatched",
		"anomalies": map[string]any{"count": 1}}); !res.IsError || !strings.Contains(text, "anomaly lane") {
		t.Fatalf("anomalies for unmatched: %s", text)
	}
	out, text = e.ok(t, "set_retention", map[string]any{"endpoint": "unmatched", "normal": map[string]any{"count": 5}})
	if !strings.HasPrefix(text, "Unmatched keeps the newest 5 requests") {
		t.Fatalf("unmatched: %s %v", text, out)
	}
	if _, text := e.ok(t, "pin_request", map[string]any{"request_id": id, "pinned": false}); !strings.Contains(text, "unpinned") {
		t.Fatalf("unpin: %s", text)
	}
	if res, text := e.call(t, context.Background(), "pin_request", map[string]any{"request_id": 9999}); !res.IsError {
		t.Fatalf("a missing request: %s", text)
	}
}

func TestDeliveryTools(t *testing.T) {
	e := newEnv(t)
	e.ok(t, "create_endpoint", map[string]any{"path": "/gh"})
	_, text := e.ok(t, "configure_endpoint", map[string]any{"endpoint": "/gh",
		"event":  map[string]any{"from": "header", "key": "X-GitHub-Delivery"},
		"checks": []any{map[string]any{"kind": "duplicates"}}})
	if !strings.Contains(text, "event") {
		t.Fatalf("configure: %s", text)
	}
	for range 2 {
		e.deliver("/gh", "{}", "X-GitHub-Delivery", "d-7")
	}
	out, text := e.ok(t, "list_requests", map[string]any{"endpoint": "/gh", "event": "d-7"})
	reqs, _ := out["requests"].([]any)
	if len(reqs) != 2 {
		t.Fatalf("event filter: %s", text)
	}
	newest, _ := reqs[0].(map[string]any)
	if newest["event"] != "d-7" || num(newest["attempt"]) != 2 || newest["verdict"] != "fail" {
		t.Fatalf("the second delivery: %v", newest)
	}
	e.deliver("/gh", "{}", "X-GitHub-Delivery", "d-8")
	e.deliver("/gh", "{}", "X-GitHub-Delivery", "d-7")
	out, text = e.ok(t, "expect", map[string]any{"endpoint": "/gh", "event": "d-7", "count": 3, "after": 0, "within_s": 5})
	if out["result"] != "pass" || !strings.HasPrefix(text, "pass: 3 request(s) with event=d-7 arrived") {
		t.Fatalf("expect three attempts of d-7: %s", text)
	}
}
