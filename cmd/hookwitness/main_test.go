package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
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

func TestKeyCheck(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "hw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, _ := secret.Load("", filepath.Join(dir, "a.key"))
	b, _ := secret.Load("", filepath.Join(dir, "b.key"))
	if same, err := st.KeyCheck(ctx, a.Check()); !same || err != nil {
		t.Fatalf("first key: %v %v", same, err)
	}
	if same, _ := st.KeyCheck(ctx, a.Check()); !same {
		t.Fatal("the same key again")
	}
	if same, _ := st.KeyCheck(ctx, b.Check()); same {
		t.Fatal("another key passed the check")
	}
}

// TestMCPCommand: the stdio bridge's flags; a wrong URL is refused before
// anything is read.
func TestMCPCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"mcp", "-h"}, func(string) string { return "" }, &out, &errOut); code != 0 ||
		!strings.Contains(errOut.String(), "-url") {
		t.Fatalf("mcp -h: %d %q", code, errOut.String())
	}
	errOut.Reset()
	if code := run(context.Background(), []string{"mcp", "-url", "127.0.0.1:8080"}, func(string) string { return "" }, &out, &errOut); code != 2 ||
		!strings.Contains(errOut.String(), "give the server's URL") {
		t.Fatalf("mcp with a bad URL: %d %q", code, errOut.String())
	}
}

// A key check that cannot run (a broken database) is logged, not skipped.
func TestCheckKeyLogsItsError(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "hw.db"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := secret.Load("", filepath.Join(dir, "hw.db.key"))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	var logs bytes.Buffer
	checkKey(context.Background(), st, keys, "hw.db.key", slog.New(slog.NewTextHandler(&logs, nil)))
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "cannot check the secret key against the database") {
		t.Fatalf("log: %s", logs.String())
	}
}

// startServe runs serve with JSON logs until the test ends and returns its
// startup line once it is logged.
func startServe(t *testing.T, args ...string) (start map[string]any, log *syncBuffer, stop func() int) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "hw.db")
	log = &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, append([]string{"serve", "-log-format", "json", "-db", db}, args...), noenv, os.Stdout, log)
	}()
	stopped := false
	stop = func() int {
		if stopped {
			return 0
		}
		stopped = true
		cancel()
		select {
		case code := <-done:
			return code
		case <-time.After(15 * time.Second):
			t.Fatal("serve did not stop")
			return -1
		}
	}
	t.Cleanup(func() { stop() })
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for line := range strings.SplitSeq(log.String(), "\n") {
			if strings.Contains(line, `"msg":"hookwitness started"`) {
				if err := json.Unmarshal([]byte(line), &start); err != nil {
					t.Fatalf("startup line is not JSON: %v", err)
				}
				return start, log, stop
			}
		}
		select {
		case code := <-done:
			done <- code
			t.Fatalf("serve exited %d before starting; log: %s", code, log.String())
		default:
		}
	}
	t.Fatalf("no startup line; log: %s", log.String())
	return nil, nil, nil
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The admin on its own address: the catch listener catches every path but
// /healthz (the prefix too), the admin answers only on its address, both
// stop together.
func TestServeWithASeparateAdmin(t *testing.T) {
	start, log, stop := startServe(t, "-addr", "127.0.0.1:0", "-admin-addr", "127.0.0.1:0")
	catchAddr, _ := start["addr"].(string)
	adminAddr, _ := start["admin_addr"].(string)
	if catchAddr == "" || adminAddr == "" || catchAddr == adminAddr {
		t.Fatalf("startup line: %v", start)
	}
	if ui, _ := start["ui"].(string); ui != "http://"+adminAddr+"/admin/" {
		t.Errorf("ui %q", ui)
	}
	if a, _ := start["api"].(string); a != "http://"+adminAddr+"/admin/api/v1" {
		t.Errorf("api %q", a)
	}
	for _, a := range []string{catchAddr, adminAddr} {
		if code, _ := get(t, "http://"+a+"/healthz"); code != 200 {
			t.Errorf("healthz on %s: %d", a, code)
		}
	}
	if code, _ := get(t, "http://"+catchAddr+"/admin/"); code != 404 {
		t.Errorf("/admin/ on the catch port: %d, want 404 (caught into Unmatched)", code)
	}
	if code, body := get(t, "http://"+adminAddr+"/admin/api/v1/unmatched/requests"); code != 200 || !strings.Contains(body, `"path": "/admin/"`) {
		t.Errorf("Unmatched on the admin: %d %s", code, body)
	}
	if code, body := get(t, "http://"+adminAddr+"/admin/"); code != 200 || !strings.Contains(body, "Hookwitness") {
		t.Errorf("UI on the admin port: %d", code)
	}
	if code, _ := get(t, "http://"+adminAddr+"/stripe"); code != 404 {
		t.Errorf("a catch path on the admin port: %d, want 404", code)
	}
	if code, _ := get(t, "http://"+adminAddr+"/admin/api/v1/unmatched/requests"); code != 200 {
		t.Errorf("second read: %d", code)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d; log: %s", code, log.String())
	}
	for _, a := range []string{catchAddr, adminAddr} {
		if c, err := net.DialTimeout("tcp", a, time.Second); err == nil {
			c.Close()
			t.Errorf("%s still accepts after the stop", a)
		}
	}
}

// The admin at the root of its own port.
func TestServeAdminAtTheRoot(t *testing.T) {
	start, _, _ := startServe(t, "-addr", "127.0.0.1:0", "-admin-addr", "127.0.0.1:0", "-admin-prefix", "/")
	adminAddr, _ := start["admin_addr"].(string)
	if ui, _ := start["ui"].(string); ui != "http://"+adminAddr+"/" {
		t.Errorf("ui %q", ui)
	}
	if code, _ := get(t, "http://"+adminAddr+"/"); code != 200 {
		t.Errorf("UI at the root: %d", code)
	}
	if code, _ := get(t, "http://"+adminAddr+"/api/v1/endpoints"); code != 200 {
		t.Errorf("API at the root: %d", code)
	}
}

// The admin's port taken: exit 1 naming it, and the catch port is free again.
func TestServeAdminAddrTaken(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	catchAddr := free.Addr().String()
	free.Close()
	var out syncBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // a serve that started anyway ends
	defer cancel()
	code := run(ctx, []string{"serve", "-log-format", "json", "-db", filepath.Join(t.TempDir(), "hw.db"),
		"-addr", catchAddr, "-admin-addr", busy.Addr().String()}, noenv, os.Stdout, &out)
	if code != 1 || !strings.Contains(out.String(), busy.Addr().String()) {
		t.Fatalf("exit %d; log: %s", code, out.String())
	}
	ln, err := net.Listen("tcp", catchAddr)
	if err != nil {
		t.Fatalf("the catch port stayed taken: %v", err)
	}
	ln.Close()
}

func TestServeSameAddressRefused(t *testing.T) {
	var out bytes.Buffer
	code := run(context.Background(), []string{"serve", "-addr", "127.0.0.1:8080", "-admin-addr", "127.0.0.1:8080"}, noenv, &out, &out)
	if got := strings.TrimSpace(out.String()); code != 2 || strings.Count(got, "\n") != 0 || !strings.Contains(got, "the same address") {
		t.Fatalf("exit %d: %q", code, got)
	}
}

// Back on a shared layout, an endpoint under the admin prefix is shadowed by
// the admin: the start says so, naming it.
func TestServeWarnsAboutShadowedEndpoints(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "hw.db")
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	st.CreateEndpoint(ctx, "/admin/stripe")
	st.CreateEndpoint(ctx, "/github")
	st.Close()
	log := &syncBuffer{}
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan int, 1)
	go func() {
		done <- run(cctx, []string{"serve", "-log-format", "json", "-db", db, "-addr", "127.0.0.1:0"}, noenv, os.Stdout, log)
	}()
	defer func() { cancel(); <-done }()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !strings.Contains(log.String(), "hookwitness started"); time.Sleep(20 * time.Millisecond) {
	}
	s := log.String()
	if !strings.Contains(s, "shadowed") || !strings.Contains(s, "/admin/stripe") || strings.Contains(s, `"/github"`) {
		t.Fatalf("log: %s", s)
	}
}
