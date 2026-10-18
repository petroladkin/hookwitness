package mcp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestReplayRequest: a signed delivery replayed to a handler that checks
// the signature passes; the replay shows on get_request; a refused target
// is a tool error naming the rule.
func TestReplayRequest(t *testing.T) {
	e := newEnv(t)
	e.ok(t, "create_endpoint", map[string]any{"path": "/github/events"})
	body := `{"action":"opened"}`
	if code := e.deliver("/github/events?from=gh", body, "X-Hub-Signature-256", githubSig("gh_test_secret", body), "Content-Type", "application/json"); code != 200 {
		t.Fatalf("deliver: %d", code)
	}
	handler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		m := hmac.New(sha256.New, []byte("gh_test_secret"))
		m.Write(b)
		if r.Header.Get("X-Hub-Signature-256") != "sha256="+hex.EncodeToString(m.Sum(nil)) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer handler.Close()
	reqs, _ := e.ok(t, "list_requests", map[string]any{"endpoint": "/github/events"})
	id := reqs["requests"].([]any)[0].(map[string]any)["id"]

	rp, text := e.ok(t, "replay_request", map[string]any{"request_id": id, "url": handler.URL + "/webhooks/github?token=x"})
	summary, _, _ := strings.Cut(text, "\n")
	if num(rp["status"]) != 202 || !strings.HasPrefix(summary, "replayed request") || !strings.Contains(summary, "202 in") || strings.Contains(summary, "token=x") {
		t.Fatalf("replay: %s", text)
	}
	got, _ := e.ok(t, "get_request", map[string]any{"id": id})
	rs, _ := got["replays"].([]any)
	if len(rs) != 1 || num(rs[0].(map[string]any)["status"]) != 202 {
		t.Fatalf("get_request replays: %v", got["replays"])
	}
	res, text := e.call(t, t.Context(), "replay_request", map[string]any{"request_id": id, "url": "http://169.254.169.254/latest/"})
	if !res.IsError || !strings.Contains(text, "target_refused") || !strings.Contains(text, "link-local") {
		t.Fatalf("metadata target: %s", text)
	}
	_, text = e.ok(t, "replay_request", map[string]any{"request_id": id, "url": "http://127.0.0.1:1/"})
	if !strings.Contains(text, "no answer (dial") {
		t.Fatalf("closed port: %s", text)
	}
}
