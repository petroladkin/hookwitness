package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParse: blocks, their directives and the indentation of a block inside
// a list item.
func TestParse(t *testing.T) {
	md := "# Page\n\n<!-- docs-check: skip: a container; really -->\n```sh\ndocker run x\n```\n\n" +
		"1. Step\n\n   <!-- docs-check: fresh; setup: a b -->\n   ```sh\n   curl -s $H   # 200\n   ```\n\n```json\n{}\n```\n"
	f := filepath.Join(t.TempDir(), "p.md")
	os.WriteFile(f, []byte(md), 0o600)
	bs, err := parse(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 2 {
		t.Fatalf("%d blocks, want the two sh ones", len(bs))
	}
	if bs[0].skip != "a container; really" || bs[0].line != 4 {
		t.Fatalf("first block %+v", bs[0])
	}
	if !bs[1].fresh || strings.Join(bs[1].setups, ",") != "a,b" || bs[1].text[0] != "curl -s $H   # 200" {
		t.Fatalf("second block %+v", bs[1])
	}
	os.WriteFile(f, []byte("<!-- docs-check: sometimes -->\n```sh\nx\n```\n"), 0o600)
	if _, err := parse(f); err == nil {
		t.Fatal("an unknown directive must be refused")
	}
}

// TestJudge: what each expectation accepts and refuses.
func TestJudge(t *testing.T) {
	req := `{"id":3,"status":401,"verdict":"fail","verdicts":[{"result":"fail","reason":"header Authorization missing"}]}`
	for _, c := range []struct {
		e  expectation
		ok bool
	}{
		{expectation{1, "status", "401", req}, true},
		{expectation{1, "status", "200", req}, false},
		{expectation{1, "fail", "Authorization missing", req}, true},
		{expectation{1, "fail", "token does not match", req}, false},
		{expectation{1, "pass", "", req}, false},
		{expectation{1, "n/v", "", req}, false},
		{expectation{1, "pass", "", ""}, false}, // nothing caught
	} {
		if got := judge(c.e) == ""; got != c.ok {
			t.Errorf("%+v: ok %v, want %v (%s)", c.e, got, c.ok, judge(c.e))
		}
	}
	dir := t.TempDir()
	out := func(want, have string) string {
		w, h := filepath.Join(dir, "w"), filepath.Join(dir, "h")
		os.WriteFile(w, []byte(want), 0o600)
		os.WriteFile(h, []byte(have), 0o600)
		return judge(expectation{1, "output", w, h})
	}
	if out("200 after 2.…", "200 after 2.004 s\n") != "" || out(`{"a":1}`, "{\"a\":1}\n") != "" {
		t.Error("a matching output is refused")
	}
	if out("200 after 2.…", "200 after 9.1 s\n") == "" || out("a\nb", "a\n") == "" {
		t.Error("a different output is accepted")
	}
}

// TestRunChecksABlock: a block runs in bash, its expectations are read
// back, and a block that checks nothing is refused before it runs.
func TestRunChecksABlock(t *testing.T) {
	setup := filepath.Join(t.TempDir(), "setup.sh")
	os.WriteFile(setup, []byte("setup_x() { X=hello; }\n"), 0o600)
	srv := &server{addr: "127.0.0.1:1"} // never called: no request expectation
	b := block{file: "p.md", line: 1, setups: []string{"x"}, text: []string{
		"echo $X   # a comment, not an expectation",
		"# hello",
		"printf 'b\\nc\\n'",
		"# b",
		"# c",
	}}
	if p := run(b, srv, setup); len(p) != 0 {
		t.Fatalf("problems %v", p)
	}
	b.text[1] = "# goodbye"
	if p := run(b, srv, setup); len(p) != 1 || !strings.Contains(p[0], `"goodbye"`) {
		t.Fatalf("a wrong output: problems %v", p)
	}
	if p := run(block{text: []string{"echo hi"}}, srv, setup); len(p) != 1 || !strings.Contains(p[0], "nothing is checked") {
		t.Fatalf("a block without checks: %v", p)
	}
}

// TestRunNoRequestForTheLine: a checked command after which nothing new
// was caught (curl failed before sending) is reported as such with its exit
// status, not judged against the request an earlier line sent.
func TestRunNoRequestForTheLine(t *testing.T) {
	setup := filepath.Join(t.TempDir(), "setup.sh")
	os.WriteFile(setup, nil, 0o600)
	// Request 1 is already caught when the block starts; nothing else ever is.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/api/v1/requests/1" {
			w.Write([]byte(`{"id":1,"status":200,"verdict":"pass","verdicts":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer api.Close()
	srv := &server{addr: strings.TrimPrefix(api.URL, "http://")}
	b := block{file: "p.md", line: 1, text: []string{
		"curl -s -o /dev/null --max-time 5 http://127.0.0.1:1/hooks   # 200",
		"curl -s -o /dev/null --max-time 5 http://127.0.0.1:1/hooks   # pass",
	}}
	p := run(b, srv, setup)
	if len(p) != 2 {
		t.Fatalf("problems %v, want one per line", p)
	}
	for i, want := range []string{"line 2: no request was caught for this line (the command exited 7)", "line 3: no request was caught for this line (the command exited 7)"} {
		if p[i] != want {
			t.Errorf("problem %d is %q, want %q", i, p[i], want)
		}
	}
}
