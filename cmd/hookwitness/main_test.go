package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
)

func noenv(string) string { return "" }

// syncBuffer lets the test read the log while the server goroutine writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestVersion(t *testing.T) {
	var out bytes.Buffer
	if code := run(context.Background(), []string{"version"}, noenv, &out, &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.HasPrefix(out.String(), "hookwitness dev (none)") {
		t.Fatalf("output %q", out.String())
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"frobnicate"}} {
		var out bytes.Buffer
		if code := run(context.Background(), args, noenv, &out, &out); code != 2 {
			t.Errorf("run(%v) exit %d, want 2", args, code)
		}
		if !strings.Contains(out.String(), "Usage: hookwitness") {
			t.Errorf("run(%v) printed no usage: %q", args, out.String())
		}
	}
}

func TestInvalidConfigIsOneLine(t *testing.T) {
	var out bytes.Buffer
	code := run(context.Background(), []string{"serve", "-log-format", "xml"}, noenv, &out, &out)
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if got := strings.TrimSpace(out.String()); strings.Count(got, "\n") != 0 || !strings.Contains(got, "want text or json") {
		t.Fatalf("want one line naming the reason, got %q", got)
	}
}

func TestDatabaseErrorExitsNonZero(t *testing.T) {
	var out bytes.Buffer
	missingDir := filepath.Join(t.TempDir(), "no", "such", "dir", "hw.db")
	if code := run(context.Background(), []string{"serve", "-addr", "127.0.0.1:0", "-db", missingDir}, noenv, &out, &out); code != 1 {
		t.Fatalf("exit %d, want 1; log: %s", code, out.String())
	}
}

// serve starts with JSON logs, answers /healthz, and exits 0 when its
// context is cancelled (what SIGINT/SIGTERM do in main).
func TestServeLifecycle(t *testing.T) {
	db := filepath.Join(t.TempDir(), "hw.db")
	var log syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"serve", "-addr", "127.0.0.1:0", "-log-format", "json"},
			func(k string) string {
				if k == "HOOKWITNESS_DB" {
					return db
				}
				return ""
			}, os.Stdout, &log)
	}()

	addrRe := regexp.MustCompile(`"addr":"([^"]+)"`)
	var addr string
	for deadline := time.Now().Add(5 * time.Second); addr == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if m := addrRe.FindStringSubmatch(log.String()); m != nil {
			addr = m[1]
		}
	}
	if addr == "" {
		t.Fatalf("no startup line; log: %s", log.String())
	}
	var start map[string]any
	if err := json.Unmarshal([]byte(strings.SplitN(log.String(), "\n", 2)[0]), &start); err != nil {
		t.Fatalf("startup line is not JSON: %v", err)
	}
	for _, k := range []string{"version", "addr", "db"} {
		if start[k] == nil {
			t.Errorf("startup line lacks %q: %v", k, start)
		}
	}

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz %d", resp.StatusCode)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d; log: %s", code, log.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}
	if !strings.Contains(log.String(), `"msg":"stopped"`) {
		t.Fatalf("no stop line; log: %s", log.String())
	}
}

func TestWarnsAboutUnreadableSecrets(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "hw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, _ := secret.Load("", filepath.Join(dir, "a.key"))
	b, _ := secret.Load("", filepath.Join(dir, "b.key"))
	ep, _ := st.CreateEndpoint(ctx, "/gh")
	cfg := emulate.Config{Default: emulate.Reply{Status: 200}, Secrets: []emulate.Secret{
		{Name: "GH", Kind: emulate.SecretKey, Sealed: a.Seal("gh_test"), Fingerprint: "x"},
		{Name: "TOK", Kind: emulate.SecretToken, Hash: a.HashToken("tok_test"), Fingerprint: "y"},
	}}
	st.SetConfig(ctx, ep.ID, cfg.String())
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	warnUnreadableSecrets(ctx, st, a, log)
	if logs.Len() != 0 {
		t.Fatalf("the right key warned: %s", logs.String())
	}
	warnUnreadableSecrets(ctx, st, b, log)
	if !strings.Contains(logs.String(), "secrets cannot be decrypted") || !strings.Contains(logs.String(), "path=/gh secrets=GH") {
		t.Fatalf("no warning: %s", logs.String())
	}
}
