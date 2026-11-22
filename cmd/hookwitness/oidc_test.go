package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/store"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// Providers at start: the start line names them, one line per provider
// gives the redirect URI to register, a warning says login is off, and the
// client secret is in no log line. A bootstrapped admin gets its email.
func TestServeWithOIDCProviders(t *testing.T) {
	dir := t.TempDir()
	const secret = "the-client-secret-of-the-test-0123"
	os.WriteFile(filepath.Join(dir, "google"), []byte(secret+"\n"), 0o600)
	env := map[string]string{
		"HOOKWITNESS_ADMIN_URL":                      "https://hooks-admin.example.test",
		"HOOKWITNESS_OIDC_PROVIDERS":                 "google",
		"HOOKWITNESS_OIDC_GOOGLE_ISSUER":             "https://accounts.google.test",
		"HOOKWITNESS_OIDC_GOOGLE_CLIENT_ID":          "gid.example",
		"HOOKWITNESS_OIDC_GOOGLE_CLIENT_SECRET_FILE": filepath.Join(dir, "google"),
		"HOOKWITNESS_OIDC_GOOGLE_ALLOWED_DOMAINS":    "example.test",
	}
	serveUntil := func(env map[string]string, db, want string) string {
		t.Helper()
		log := &syncBuffer{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan int, 1)
		go func() {
			done <- run(ctx, []string{"serve", "-log-format", "json", "-db", db, "-addr", "127.0.0.1:0"}, envOf(env), os.Stdout, log)
		}()
		for deadline := time.Now().Add(5 * time.Second); !strings.Contains(log.String(), want); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				cancel()
				t.Fatalf("no %q in the log: %s", want, log.String())
			}
		}
		cancel()
		<-done
		return log.String()
	}
	log := serveUntil(env, filepath.Join(dir, "hw.db"), "login is off")
	for _, want := range []string{`"oidc":"google"`, `"msg":"oidc provider configured"`, `"name":"google"`, `"client_id":"gid.example"`,
		`"redirect_uri":"https://hooks-admin.example.test/admin/oidc/google/callback"`, `"allowed_domains":"example.test"`,
		"OIDC providers are configured but login is off"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %s:\n%s", want, log)
		}
	}
	if strings.Contains(log, secret) {
		t.Fatal("the client secret is in the log")
	}

	// The bootstrapped admin with an email: no login-off warning then.
	os.WriteFile(filepath.Join(dir, "pw"), []byte("correct horse battery staple\n"), 0o600)
	env["HOOKWITNESS_ADMIN_USER"], env["HOOKWITNESS_ADMIN_PASSWORD_FILE"], env["HOOKWITNESS_ADMIN_EMAIL"] = "ann", filepath.Join(dir, "pw"), "Ann@Example.test"
	db := filepath.Join(dir, "boot.db")
	log = serveUntil(env, db, "oidc provider configured")
	if strings.Contains(log, "login is off") || strings.Contains(log, secret) {
		t.Fatalf("with an admin: %s", log)
	}
	st, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.UserByName(context.Background(), "ann")
	st.Close()
	if err != nil || u.Email != "ann@example.test" || u.Role != store.RoleAdmin {
		t.Fatalf("the bootstrapped admin: %+v %v", u, err)
	}

	// A secret file that is missing: exit 2, one line naming the variable.
	env["HOOKWITNESS_OIDC_GOOGLE_CLIENT_SECRET_FILE"] = filepath.Join(dir, "missing")
	var out bytes.Buffer
	if code := run(context.Background(), []string{"serve", "-db", filepath.Join(dir, "x.db")}, envOf(env), &out, &out); code != 2 ||
		strings.Count(strings.TrimSpace(out.String()), "\n") != 0 || !strings.Contains(out.String(), "HOOKWITNESS_OIDC_GOOGLE_CLIENT_SECRET_FILE") {
		t.Fatalf("a missing secret file: exit %d, %q", code, out.String())
	}
}
