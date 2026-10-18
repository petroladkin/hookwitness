package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
)

const setupFile = `{"hookwitness": 1, "endpoints": [
  {"path": "/orders", "default": {"status": 202, "body": "ok"},
   "checks": [{"kind": "bearer", "secret": "TOK", "enforce": true}],
   "secrets": [{"name": "TOK", "kind": "token"}]},
  {"path": "/refunds", "default": {"status": 200}}]}
`

func endpointConfigs(t *testing.T, db string) map[string]string {
	t.Helper()
	st, err := store.Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	eps, err := st.Endpoints(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range eps {
		out[e.Path] = e.Config
	}
	return out
}

func TestServeImport(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "hw.db")
	f := writeFile(t, dir, "setup.json", setupFile, 0o644)
	_, log, stop, code := serveWith(t, db, map[string]string{"HOOKWITNESS_IMPORT": f})
	if code != 0 {
		t.Fatalf("serve: %d %s", code, log)
	}
	if !strings.Contains(log.String(), `"msg":"endpoint created from a setup file","path":"/orders","secrets_not_set":"TOK"`) {
		t.Fatalf("log: %s", log)
	}
	stop()
	first := endpointConfigs(t, db)
	if len(first) != 2 || !strings.Contains(first["/orders"], `"status":202`) {
		t.Fatalf("endpoints %v", first)
	}

	// The value set meanwhile survives a restart with the same file.
	st, _ := store.Open(context.Background(), db)
	keys, _ := secret.Load("", db+".key")
	id, cfg, _, _ := st.EndpointByPath(context.Background(), "/orders")
	c, _ := emulate.Parse(cfg)
	c.Secrets[0], _ = emulate.SealSecret("TOK", emulate.SecretToken, "tok_value_test", keys)
	st.SetConfig(context.Background(), id, c.String())
	st.Close()
	before := endpointConfigs(t, db)
	_, log, stop, code = serveWith(t, db, nil, "-import", f)
	if code != 0 || !strings.Contains(log.String(), `"msg":"setup applied","file":"`+f+`","created":0,"replaced":0,"unchanged":2`) {
		t.Fatalf("restart: %d %s", code, log)
	}
	stop()
	if after := endpointConfigs(t, db); after["/orders"] != before["/orders"] {
		t.Fatalf("a restart with the same file changed /orders:\n%s\n%s", before["/orders"], after["/orders"])
	}

	// A bad file stops the start with one line naming the problem.
	bad := writeFile(t, dir, "bad.json", `{"hookwitness": 9, "endpoints": []}`, 0o644)
	_, log, _, code = serveWith(t, filepath.Join(dir, "other.db"), nil, "-import", bad)
	if code != 1 || !strings.Contains(log.String(), "newer hookwitness") {
		t.Fatalf("bad file: %d %s", code, log)
	}
	_, log, _, code = serveWith(t, filepath.Join(dir, "other.db"), nil, "-import", filepath.Join(dir, "missing.json"))
	if code != 1 || !strings.Contains(log.String(), "cannot read the setup file") {
		t.Fatalf("missing file: %d %s", code, log)
	}
}

func applyRun(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), append([]string{"apply"}, args...), noenv, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestApplyCommand(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "hw.db")
	f := writeFile(t, dir, "setup.json", setupFile, 0o644)
	start, _, _, code := serveWith(t, db, nil)
	if code != 0 {
		t.Fatal("serve did not start")
	}
	url := "http://" + start["addr"].(string)

	code, out, errOut := applyRun("-url", url, f)
	if code != 0 || out != "created /orders (secrets not set: TOK)\ncreated /refunds\n" {
		t.Fatalf("apply: %d %q %q", code, out, errOut)
	}
	code, out, _ = applyRun("-url", url, f)
	if code != 0 || out != "unchanged /orders (secrets not set: TOK)\nunchanged /refunds\n" {
		t.Fatalf("apply again: %d %q", code, out)
	}
	changed := writeFile(t, dir, "changed.json", strings.Replace(setupFile, "202", "204", 1), 0o644)
	code, out, _ = applyRun("-url", url+"/admin/api/v1", changed)
	if code != 0 || !strings.HasPrefix(out, "replaced /orders") {
		t.Fatalf("apply a change: %d %q", code, out)
	}

	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{nil, 2, "give one setup file"},
		{[]string{"-url", "ftp://x", f}, 2, "--url"},
		{[]string{"-url", url, filepath.Join(dir, "missing.json")}, 2, "missing.json"},
		{[]string{"-url", url, writeFile(t, dir, "bad.json", `{"hookwitness":1,"endpoints":[{"path":"nope","default":{"status":200}}]}`, 0o644)}, 1, "The path must start"},
		{[]string{"-url", "http://127.0.0.1:1", f}, 1, "cannot reach"},
	} {
		code, _, errOut := applyRun(c.args...)
		if code != c.code || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: %d %q, want %d and %q", c.args, code, errOut, c.code, c.want)
		}
	}
}

func TestApplyWithLogin(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "hw.db")
	pw := writeFile(t, dir, "admin.pw", testAdminPW, 0o600)
	start, _, _, code := serveWith(t, db, map[string]string{"HOOKWITNESS_ADMIN_USER": "root", "HOOKWITNESS_ADMIN_PASSWORD_FILE": pw})
	if code != 0 {
		t.Fatal("serve did not start")
	}
	url := "http://" + start["addr"].(string)
	ctx := context.Background()
	st, _ := store.Open(ctx, db)
	keys, _ := secret.Load("", db+".key")
	// A user (not an admin) may import.
	qa, err := st.AddUser(ctx, "qa", store.RoleUser, "unused-hash")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := access.New(st, keys, "").MintFor(ctx, qa.ID, "ci")
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	f := writeFile(t, dir, "setup.json", setupFile, 0o644)
	code, _, errOut := applyRun("-url", url, f)
	if code != 1 || !strings.Contains(errOut, "-token-file") {
		t.Fatalf("without a token: %d %q", code, errOut)
	}
	tf := writeFile(t, dir, "token", tok+"\n", 0o600)
	code, out, errOut := applyRun("-url", url, "-token-file", tf, f)
	if code != 0 || !strings.HasPrefix(out, "created /orders") {
		t.Fatalf("with a user's token: %d %q %q", code, out, errOut)
	}
	if strings.Contains(out+errOut, tok) {
		t.Fatal("the token was printed")
	}
}
