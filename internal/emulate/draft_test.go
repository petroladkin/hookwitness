package emulate

import "testing"

func TestSchemaName(t *testing.T) {
	for in, want := range map[string]string{
		"order.paid": "OrderPaid", "pull_request": "PullRequest", "hooks": "Hooks", "": "Payload", "...": "Payload",
		"2fa.enabled": "Schema2faEnabled", "v2": "V2", "café-paid": "CafPaid", "X-GitHub-Event": "XGitHubEvent",
	} {
		if got := SchemaName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestDraftHint(t *testing.T) {
	cfg := schemaConfig(Check{Kind: KindSchema, Select: byType()})
	cfg.Checks = append([]Check{{Kind: KindSchema, Schema: "OrderPaid"}}, cfg.Checks...)
	req := func(body string) Request { return Request{Body: []byte(body)} }
	for _, tc := range []struct {
		reqs []Request
		want DraftHint
	}{
		{[]Request{req(`{"type":"order.shipped"}`)}, DraftHint{Name: "OrderShipped", Check: 2, Label: "JSON Schema by /type", Value: "order.shipped"}},
		{[]Request{req(`{"type":"order.paid"}`), req(`{"type":"order.paid"}`)}, DraftHint{Name: "OrderPaid", Check: 2, Label: "JSON Schema by /type", Value: "order.paid", Mapped: "OrderPaid"}},
		{[]Request{req(`{"type":"a"}`), req(`{"type":"b"}`)}, DraftHint{Name: "Events", Check: 2, Label: "JSON Schema by /type"}},
		{[]Request{req(`{"no":"type"}`)}, DraftHint{Name: "Events", Check: 2, Label: "JSON Schema by /type"}},
	} {
		if got := cfg.DraftHint("/shop/events/", tc.reqs); got != tc.want {
			t.Errorf("%+v, want %+v", got, tc.want)
		}
	}
	if got := (Config{}).DraftHint("/github", []Request{req(`{}`)}); got != (DraftHint{Name: "Github"}) {
		t.Errorf("no checks: %+v", got)
	}
	if !cfg.MapValue(2, "order.shipped", "OrderShipped") || cfg.MapValue(2, "order.shipped", "X") || cfg.MapValue(1, "v", "X") || cfg.MapValue(3, "v", "X") {
		t.Fatal("MapValue")
	}
	if m := cfg.Checks[1].Select.Map; m[len(m)-1] != (Mapping{"order.shipped", "OrderShipped"}) {
		t.Fatalf("map %+v", m)
	}
}
