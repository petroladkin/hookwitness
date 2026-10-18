package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
)

const testAdminPW = "bootstrap-password-42"

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func writeFile(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, mode)
	return p
}

// serveWith runs serve on db with env until stopped; it returns the log and
// the startup line, or the exit code when serve did not start.
func serveWith(t *testing.T, db string, env map[string]string, args ...string) (start map[string]any, log *syncBuffer, stop func() int, code int) {
	t.Helper()
	log = &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, append([]string{"serve", "-log-format", "json", "-db", db, "-addr", "127.0.0.1:0"}, args...), envMap(env), os.Stdout, log)
	}()
	stopped := false
	stop = func() int {
		if stopped {
			return 0
		}
		stopped = true
		cancel()
		select {
		case c := <-done:
			return c
		case <-time.After(15 * time.Second):
			t.Fatal("serve did not stop")
			return -1
		}
	}
	t.Cleanup(func() { stop() })
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for line := range strings.SplitSeq(log.String(), "\n") {
			if strings.Contains(line, `"msg":"hookwitness started"`) {
				json.Unmarshal([]byte(line), &start)
				// The lines after the start are written at once; wait for them.
				time.Sleep(50 * time.Millisecond)
				return start, log, stop, 0
			}
		}
		select {
		case c := <-done:
			stopped = true
			return nil, log, stop, c
		default:
		}
	}
	t.Fatalf("no startup line; log: %s", log.String())
	return
}

func TestBootstrapFirstAdmin(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "hw.db")
	pw := writeFile(t, dir, "admin.pw", testAdminPW+"\n", 0o600)
	env := map[string]string{"HOOKWITNESS_ADMIN_USER": "Root", "HOOKWITNESS_ADMIN_PASSWORD_FILE": pw}
	start, log, stop, code := serveWith(t, db, env)
	if code != 0 || start["login"] != "on" || !strings.Contains(log.String(), "admin user created from HOOKWITNESS_ADMIN_USER") || !strings.Contains(log.String(), `"user":"root"`) {
		t.Fatalf("first start: %d %v; log: %s", code, start, log.String())
	}
	addr := start["addr"].(string)
	form := url.Values{"name": {"root"}, "password": {testAdminPW}}
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cl.PostForm("http://"+addr+"/admin/login", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login as the bootstrapped admin: %d", resp.StatusCode)
	}
	if code, _ := get(t, "http://"+addr+"/admin/api/v1/endpoints"); code != 401 {
		t.Fatalf("API without credentials: %d", code)
	}
	if code, _ := get(t, "http://"+addr+"/healthz"); code != 200 {
		t.Fatalf("healthz with login on: %d", code)
	}
	if code, _ := get(t, "http://"+addr+"/any/catch/path"); code != 404 {
		t.Fatalf("a catch path with login on: %d", code)
	}
	stop()

	// A restart with an edited file creates nothing and keeps the password.
	writeFile(t, dir, "admin.pw", "another-password-77\n", 0o600)
	start, log, _, code = serveWith(t, db, env)
	if code != 0 || !strings.Contains(log.String(), "admin bootstrap skipped") || start["users"] != float64(1) {
		t.Fatalf("second start: %d %v; log: %s", code, start, log.String())
	}
	resp, err = cl.PostForm("http://"+start["addr"].(string)+"/admin/login", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the first password after an edited file: %d", resp.StatusCode)
	}
	if strings.Contains(log.String(), testAdminPW) {
		t.Fatal("the password reached the log")
	}
}

func TestBootstrapRefusesBadSettings(t *testing.T) {
	dir := t.TempDir()
	short := writeFile(t, dir, "short.pw", "short\n", 0o600)
	good := writeFile(t, dir, "good.pw", testAdminPW, 0o600)
	for name, env := range map[string]map[string]string{
		"user only":      {"HOOKWITNESS_ADMIN_USER": "root"},
		"file only":      {"HOOKWITNESS_ADMIN_PASSWORD_FILE": good},
		"short password": {"HOOKWITNESS_ADMIN_USER": "root", "HOOKWITNESS_ADMIN_PASSWORD_FILE": short},
		"missing file":   {"HOOKWITNESS_ADMIN_USER": "root", "HOOKWITNESS_ADMIN_PASSWORD_FILE": filepath.Join(dir, "none")},
		"bad name":       {"HOOKWITNESS_ADMIN_USER": "root admin", "HOOKWITNESS_ADMIN_PASSWORD_FILE": good},
		"short token":    {"HOOKWITNESS_API_TOKEN_FILE": short},
	} {
		_, log, _, code := serveWith(t, filepath.Join(t.TempDir(), "hw.db"), env)
		if code != 1 {
			t.Errorf("%s: exit %d; log: %s", name, code, log.String())
		}
	}
}

// The machine token closes the API and /mcp even without users; the UI
// stays open and says so.
func TestMachineTokenFile(t *testing.T) {
	dir := t.TempDir()
	const tok = "hkw_machine_0123456789abcdef0123456789abcdef"
	f := writeFile(t, dir, "token", tok+"\n", 0o600)
	start, log, _, code := serveWith(t, filepath.Join(dir, "hw.db"), map[string]string{"HOOKWITNESS_API_TOKEN_FILE": f})
	if code != 0 || start["machine_token"] != "on" || start["login"] != "off" {
		t.Fatalf("start: %d %v; log %s", code, start, log.String())
	}
	addr := start["addr"].(string)
	if code, _ := get(t, "http://"+addr+"/admin/api/v1/endpoints"); code != 401 {
		t.Fatalf("API without the token: %d", code)
	}
	req, _ := http.NewRequest("GET", "http://"+addr+"/admin/api/v1/endpoints", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("API with the machine token: %d", resp.StatusCode)
	}
	if code, body := get(t, "http://"+addr+"/admin/"); code != 200 || !strings.Contains(body, "No login.") {
		t.Fatalf("UI: %d", code)
	}
	if strings.Contains(log.String(), tok) {
		t.Fatal("the machine token reached the log")
	}
}

func TestWarnsWhenOpenBeyondLoopback(t *testing.T) {
	_, log, _, _ := serveWith(t, filepath.Join(t.TempDir(), "hw.db"), nil, "-addr", "0.0.0.0:0")
	if !strings.Contains(log.String(), "the admin is open without login") {
		t.Fatalf("no warning: %s", log.String())
	}
	_, log, _, _ = serveWith(t, filepath.Join(t.TempDir(), "hw.db"), nil)
	if strings.Contains(log.String(), "the admin is open without login") {
		t.Fatalf("a warning on loopback: %s", log.String())
	}
}

// userRun runs hookwitness user with stdin.
func userRun(t *testing.T, db, stdin string, args ...string) (code int, out, errOut string) {
	t.Helper()
	old, oldTerm := userIn, userTerminal
	userIn, userTerminal = strings.NewReader(stdin), func() bool { return false }
	defer func() { userIn, userTerminal = old, oldTerm }()
	var o, e bytes.Buffer
	code = run(context.Background(), append([]string{"user"}, append(args, "-db", db)...), noenv, &o, &e)
	return code, o.String(), e.String()
}

func TestUserCommand(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "hw.db")
	// No database and key yet: refused, and no key is made.
	if code, _, e := userRun(t, db, testAdminPW, "add", "root", "-admin"); code != exitFailed || !strings.Contains(e, "no secret key") {
		t.Fatalf("without a key: %d %s", code, e)
	}
	if _, err := os.Stat(db + ".key"); err == nil {
		t.Fatal("the user command made a key")
	}
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := secret.Load("", db+".key")
	st.Close()

	for _, c := range []struct {
		stdin string
		args  []string
		code  int
		msg   string
	}{
		{"", nil, exitUsage, "Usage"},
		{"", []string{"frob"}, exitUsage, "unknown command"},
		{"", []string{"add"}, exitUsage, "name the user"},
		{testAdminPW, []string{"add", "bad name"}, exitUsage, "a name is"},
		{"short\n", []string{"add", "root", "-admin"}, exitUsage, "at least 12"},
		{testAdminPW, []string{"add", "root"}, exitRefused, "the first user must be an admin"},
		{testAdminPW + "\n", []string{"add", "Root", "-admin"}, exitOK, "added root (admin)"},
		{testAdminPW, []string{"add", "root"}, exitRefused, "exists"},
		{"user-password-5678", []string{"add", "bob"}, exitOK, "added bob (user)"},
		{"", []string{"rm", "root"}, exitRefused, "last admin"},
		{"", []string{"rm", "nobody"}, exitNoUser, "no user nobody"},
		{"x-password-12345", []string{"passwd", "nobody"}, exitNoUser, "no user nobody"},
	} {
		code, _, e := userRun(t, db, c.stdin, c.args...)
		if code != c.code || !strings.Contains(e, c.msg) {
			t.Errorf("user %v: exit %d %q, want %d %q", c.args, code, e, c.code, c.msg)
		}
	}
	code, out, _ := userRun(t, db, "", "list", "-json")
	var list []map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &list) != nil || len(list) != 2 || list[0]["name"] != "bob" || list[1]["role"] != "admin" {
		t.Fatalf("list -json: %d %s", code, out)
	}
	if code, out, _ := userRun(t, db, "", "list"); code != 0 || !strings.Contains(out, "NAME") || !strings.Contains(out, "root") {
		t.Fatalf("list: %d %s", code, out)
	}

	// Recovery: a lost admin password is reset from the CLI; logins end.
	st, _ = store.Open(ctx, db)
	defer st.Close()
	auth := access.New(st, keys, "")
	u, _ := st.UserByName(ctx, "root")
	sid, _, _ := auth.StartSession(ctx, u)
	pwf := writeFile(t, dir, "new.pw", "recovered-password-1\n", 0o600)
	if code, _, e := userRun(t, db, "", "passwd", "root", "-password-file", pwf); code != 0 || !strings.Contains(e, "logins have ended") {
		t.Fatalf("passwd: %d %s", code, e)
	}
	if _, err := auth.Login(ctx, "root", "recovered-password-1"); err != nil {
		t.Fatalf("login with the recovered password: %v", err)
	}
	if _, err := auth.Session(ctx, sid); err == nil {
		t.Fatal("a session survived passwd")
	}
	if code, _, _ := userRun(t, db, "", "rm", "bob"); code != 0 {
		t.Fatalf("rm bob: %d", code)
	}
}

// The MCP bridge with -token-file reaches a server with login on; without
// it, it says what is missing.
func TestMCPBridgeWithToken(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "hw.db")
	pw := writeFile(t, dir, "admin.pw", testAdminPW, 0o600)
	start, _, _, code := serveWith(t, db, map[string]string{"HOOKWITNESS_ADMIN_USER": "root", "HOOKWITNESS_ADMIN_PASSWORD_FILE": pw})
	if code != 0 {
		t.Fatal("serve did not start")
	}
	addr := start["addr"].(string)
	// A token for root, made through the store as the account page would.
	ctx := context.Background()
	st, _ := store.Open(ctx, db)
	keys, _ := secret.Load("", db+".key")
	u, _ := st.UserByName(ctx, "root")
	tok, _, err := access.New(st, keys, "").MintFor(ctx, u.ID, "agent")
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	tf := writeFile(t, dir, "token", tok+"\n", 0o600)

	bridge := func(args ...string) (int, string, *sdk.ClientSession) {
		toBridge, clientOut := io.Pipe()
		clientIn, fromBridge := io.Pipe()
		oldIn, oldOut := bridgeIn, bridgeOut
		bridgeIn, bridgeOut = toBridge, fromBridge
		t.Cleanup(func() { bridgeIn, bridgeOut = oldIn, oldOut })
		var errOut syncBuffer
		done := make(chan int, 1)
		go func() {
			done <- run(context.Background(), append([]string{"mcp", "-url", "http://" + addr}, args...), noenv, io.Discard, &errOut)
			fromBridge.Close()
		}()
		cl := sdk.NewClient(&sdk.Implementation{Name: "stdio", Version: "1"}, nil)
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cs, err := cl.Connect(cctx, &sdk.IOTransport{Reader: clientIn, Writer: clientOut}, nil)
		if err != nil {
			clientOut.Close()
			select {
			case c := <-done:
				return c, errOut.String(), nil
			case <-time.After(5 * time.Second):
				return -1, errOut.String(), nil
			}
		}
		t.Cleanup(func() { cs.Close() })
		return 0, "", cs
	}
	code, errOut, _ := bridge()
	if code != 1 || !strings.Contains(errOut, "-token-file") {
		t.Fatalf("bridge without a token: %d %q", code, errOut)
	}
	_, _, cs := bridge("-token-file", tf)
	if cs == nil {
		t.Fatal("bridge with a token did not connect")
	}
	if list, err := cs.ListTools(context.Background(), nil); err != nil || len(list.Tools) == 0 {
		t.Fatalf("tools through the bridge with a token: %v", err)
	}
}
