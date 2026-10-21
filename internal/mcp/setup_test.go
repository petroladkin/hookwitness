package mcp

import (
	"context"
	"strings"
	"testing"
)

func TestExportAndImport(t *testing.T) {
	e := newEnv(t)
	e.ok(t, "create_endpoint", map[string]any{"path": "/gh"})
	e.ok(t, "set_secret", map[string]any{"endpoint": "/gh", "name": "GH", "kind": "key", "value": "gh_mcp_test_value"})
	e.ok(t, "configure_endpoint", map[string]any{"endpoint": "/gh", "default": map[string]any{"status": 202},
		"checks": []any{map[string]any{"kind": "github", "secret": "GH", "enforce": true}}})

	out, text := e.ok(t, "export_endpoint", map[string]any{"endpoints": []any{"/gh"}})
	if strings.Contains(text, "gh_mcp_test_value") || strings.Contains(text, "fingerprint") {
		t.Fatalf("the export carries the secret: %s", text)
	}
	if !strings.Contains(text, "gh.hookwitness.json") || !strings.Contains(text, "values are not in it") {
		t.Fatalf("summary: %s", text)
	}
	file := out["setup"]
	if m, _ := file.(map[string]any); m["hookwitness"] != float64(1) {
		t.Fatalf("setup %v", file)
	}
	if _, text := e.ok(t, "export_endpoint", map[string]any{}); !strings.Contains(text, "1 endpoint") {
		t.Fatalf("export all: %s", text)
	}

	// Into another instance: created, the secret not set.
	o := newEnv(t)
	_, text = o.ok(t, "import_endpoints", map[string]any{"setup": file})
	if !strings.Contains(text, "created /gh") || !strings.Contains(text, "not set: GH") || !strings.Contains(text, "set_secret") {
		t.Fatalf("import: %s", text)
	}
	// Taken: refused without on_conflict replace, then unchanged with it.
	if res, text := o.call(t, context.Background(), "import_endpoints", map[string]any{"setup": file}); !res.IsError || !strings.Contains(text, "already an endpoint") {
		t.Fatalf("taken: %s", text)
	}
	if _, text := o.ok(t, "import_endpoints", map[string]any{"setup": file, "on_conflict": "replace"}); !strings.Contains(text, "unchanged /gh") {
		t.Fatalf("replace: %s", text)
	}
	// The enforced check over an unset secret lets the delivery through.
	if code := o.deliver("/gh", `{}`, "X-Hub-Signature-256", githubSig("gh_mcp_test_value", `{}`)); code != 202 {
		t.Fatalf("delivery: %d", code)
	}
	if res, text := o.call(t, context.Background(), "import_endpoints", map[string]any{"setup": map[string]any{"hookwitness": 5, "endpoints": []any{}}}); !res.IsError || !strings.Contains(text, "newer hookwitness") {
		t.Fatalf("newer: %s", text)
	}
}
