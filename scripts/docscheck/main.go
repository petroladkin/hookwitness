// Command docscheck runs the shell examples of the documentation against a
// built binary and checks the results the text promises.
//
// Usage: go run ./scripts/docscheck -bin bin/hookwitness README.md docs/*.md
//
// Every ```sh block is run with bash (needs curl, jq, openssl), each page
// against its own fresh server; 127.0.0.1:8080 and localhost:8080 in a
// block are replaced by that server's address. A block is checked through
// comments the reader sees anyway:
//
//   - a command ending in `# pass`, `# fail: <reason>`, `# n/v: <reason>` or
//     `# n/c: <reason>`: the newest caught request has that verdict (pass:
//     the request's overall verdict; otherwise one of its verdicts has that
//     result and a reason containing <reason>, a trailing … dropped);
//   - a command ending in `# 200` (any three-digit status, words may follow):
//     the newest caught request was answered with that status;
//   - comment lines right after a command without such a comment: the
//     command's output, line by line; a line ending in … matches a prefix.
//
// An HTML comment on the line before the block steers it:
// `<!-- docs-check: skip: <why> -->` (e.g. Docker, a long-running server;
// the rest of the comment is the reason) or
// `<!-- docs-check: setup: <name> [<name>...] -->`, which first runs the
// functions setup_<name> of setup.sh: what the page tells the reader to do
// in the UI, done through the API; `fresh` (`fresh; setup: …`) runs the
// block on a server of its own, for examples that count requests from 1.
// A block that is run must check something.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type block struct {
	file   string
	line   int // of the opening fence
	text   []string
	skip   string
	setups []string
	fresh  bool // on a server of its own: the example counts requests from 1
}

var (
	directive = regexp.MustCompile(`^<!--\s*docs-check:\s*(.*?)\s*-->$`)
	verdictC  = regexp.MustCompile(`\s#\s+(pass|fail|n/v|n/c)(?::\s*(.*))?$`)
	statusC   = regexp.MustCompile(`\s#\s+([1-5][0-9][0-9])\b.*$`)
	outputC   = regexp.MustCompile(`^#(?: (.*))?$`)
	trailingC = regexp.MustCompile(`\s+#\s[^'"]*$`)
)

func main() {
	bin := flag.String("bin", "bin/hookwitness", "the hookwitness binary")
	flag.Parse()
	setup, err := filepath.Abs("scripts/docscheck/setup.sh")
	if err != nil {
		fatal(err)
	}
	failed, ran, skipped := 0, 0, 0
	for _, f := range flag.Args() {
		blocks, err := parse(f)
		if err != nil {
			fatal(err)
		}
		if len(blocks) == 0 {
			continue
		}
		var srv *server
		for _, b := range blocks {
			if b.skip != "" {
				fmt.Printf("SKIP %s:%d  %s\n", b.file, b.line, b.skip)
				skipped++
				continue
			}
			if srv != nil && b.fresh {
				srv.stop()
				srv = nil
			}
			if srv == nil {
				if srv, err = start(*bin); err != nil {
					fatal(err)
				}
			}
			ran++
			problems := run(b, srv, setup)
			if b.fresh { // the next block must not see this one's requests
				srv.stop()
				srv = nil
			}
			if len(problems) == 0 {
				fmt.Printf("ok   %s:%d\n", b.file, b.line)
				continue
			}
			failed++
			fmt.Printf("FAIL %s:%d\n", b.file, b.line)
			for _, p := range problems {
				fmt.Printf("     %s\n", p)
			}
		}
		if srv != nil {
			srv.stop()
		}
	}
	fmt.Printf("%d blocks run, %d skipped, %d failed\n", ran, skipped, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "docscheck:", err)
	os.Exit(2)
}

// parse finds the ```sh blocks of a Markdown file and their directives.
func parse(file string) ([]block, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	var out []block
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "```sh" {
			continue
		}
		b := block{file: file, line: i + 1}
		indent := lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " "))]
		for j := i - 1; j >= 0 && strings.TrimSpace(lines[j]) != ""; j-- {
			m := directive.FindStringSubmatch(strings.TrimSpace(lines[j]))
			if m == nil {
				break
			}
			if why, ok := strings.CutPrefix(m[1], "skip:"); ok { // the reason may hold anything
				b.skip = strings.TrimSpace(why)
				continue
			}
			for _, d := range strings.Split(m[1], ";") {
				d = strings.TrimSpace(d)
				switch {
				case d == "fresh":
					b.fresh = true
				case strings.HasPrefix(d, "setup:"):
					b.setups = append(b.setups, strings.Fields(strings.TrimPrefix(d, "setup:"))...)
				default:
					return nil, fmt.Errorf("%s:%d: unknown directive %q", file, j+1, d)
				}
			}
		}
		for i++; i < len(lines) && strings.TrimSpace(lines[i]) != "```"; i++ {
			b.text = append(b.text, strings.TrimPrefix(lines[i], indent))
		}
		out = append(out, b)
	}
	return out, nil
}

type server struct {
	cmd  *exec.Cmd
	dir  string
	addr string
}

func start(bin string) (*server, error) {
	dir, err := os.MkdirTemp("", "docscheck-")
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	addr := l.Addr().String()
	l.Close()
	cmd := exec.Command(bin, "serve", "-addr", addr, "-db", filepath.Join(dir, "hw.db"), "-max-wait", "10s")
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	for i := 0; i < 100; i++ {
		if r, err := http.Get("http://" + addr + "/healthz"); err == nil {
			r.Body.Close()
			return &server{cmd, dir, addr}, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	cmd.Process.Kill()
	return nil, fmt.Errorf("hookwitness did not start on %s", addr)
}

func (s *server) stop() {
	s.cmd.Process.Kill()
	s.cmd.Wait()
	os.RemoveAll(s.dir)
}

// prelude defines what the instrumented block calls: __expect records the
// newest caught request next to an expectation, __output the output of a
// command next to the lines the text shows.
const prelude = `
__hw_last=0
__newest() {
  while curl -fs -o /dev/null "$__api/requests/$((__hw_last + 1))"; do __hw_last=$((__hw_last + 1)); done
}
__expect() { # line kind arg
  __newest
  printf '%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$(curl -fs "$__api/requests/$__hw_last" | jq -c . 2>/dev/null)" >> "$__log"
}
__output() { # line expected-file
  printf '%s\toutput\t%s\t%s\n' "$1" "$2" "$__out.$1" >> "$__log"
}
`

type expectation struct {
	line             int
	kind, arg, extra string
}

// run instruments one block, runs it and returns what did not hold.
func run(b block, s *server, setup string) []string {
	dir, err := os.MkdirTemp("", "docscheck-block-")
	if err != nil {
		return []string{err.Error()}
	}
	defer os.RemoveAll(dir)
	var src strings.Builder
	fmt.Fprintf(&src, "__api=http://%s/admin/api/v1\n__log=%s\n__out=%s\n", s.addr, filepath.Join(dir, "log"), filepath.Join(dir, "out"))
	src.WriteString(prelude)
	fmt.Fprintf(&src, "source %q\nHW=http://%s\n", setup, s.addr)
	for _, name := range b.setups {
		fmt.Fprintf(&src, "setup_%s || { echo 'setup %s failed' >&2; exit 97; }\n", name, name)
	}
	src.WriteString("__hw_last=0; __newest\n")
	text := make([]string, len(b.text))
	for i, l := range b.text {
		l = strings.ReplaceAll(l, "127.0.0.1:8080", s.addr)
		text[i] = strings.ReplaceAll(l, "localhost:8080", s.addr)
	}
	checks := 0
	for i := 0; i < len(text); i++ {
		l := text[i]
		n := b.line + 1 + i
		continued := strings.HasSuffix(strings.TrimRight(l, " "), "\\")
		isComment := strings.HasPrefix(strings.TrimSpace(l), "#")
		if continued || isComment || strings.TrimSpace(l) == "" {
			src.WriteString(l + "\n")
			continue
		}
		if m := verdictC.FindStringSubmatch(l); m != nil {
			fmt.Fprintf(&src, "%s\n__expect %d %q %q\n", l, n, m[1], strings.TrimSuffix(strings.TrimSpace(m[2]), "…"))
			checks++
			continue
		}
		if m := statusC.FindStringSubmatch(l); m != nil {
			fmt.Fprintf(&src, "%s\n__expect %d status %q\n", l, n, m[1])
			checks++
			continue
		}
		var want []string
		for j := i + 1; j < len(text); j++ {
			m := outputC.FindStringSubmatch(text[j])
			if m == nil {
				break
			}
			want = append(want, m[1])
		}
		if len(want) == 0 {
			src.WriteString(l + "\n")
			continue
		}
		file := filepath.Join(dir, "want."+strconv.Itoa(n))
		os.WriteFile(file, []byte(strings.Join(want, "\n")), 0o600)
		cmdText, comment := l, ""
		if loc := trailingC.FindStringIndex(l); loc != nil { // the redirect goes before a comment
			cmdText, comment = l[:loc[0]], l[loc[0]:]
		}
		fmt.Fprintf(&src, "%s > \"$__out.%d\"%s\n__output %d %q\n", cmdText, n, comment, n, file)
		checks++
		i += len(want)
	}
	if checks == 0 {
		return []string{"nothing is checked: add an expected verdict, status or output, or skip the block"}
	}
	cmd := exec.Command("bash", "-c", src.String())
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = nil, &stderr
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return []string{err.Error()}
	}
	go func() { done <- cmd.Wait() }()
	var problems []string
	select {
	case err := <-done:
		if code := cmd.ProcessState.ExitCode(); code == 97 {
			problems = append(problems, "setup failed: "+strings.TrimSpace(stderr.String()))
		} else if err != nil && !isExit(err) {
			problems = append(problems, err.Error())
		}
	case <-time.After(2 * time.Minute):
		cmd.Process.Kill()
		problems = append(problems, "did not finish in 2 minutes")
	}
	for _, l := range strings.Split(stderr.String(), "\n") {
		if strings.Contains(l, "command not found") || strings.Contains(l, "syntax error") || strings.Contains(l, "No such file") {
			problems = append(problems, "bash: "+l)
		}
	}
	got := 0
	if f, err := os.Open(filepath.Join(dir, "log")); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 16<<20)
		for sc.Scan() {
			p := strings.SplitN(sc.Text(), "\t", 4)
			if len(p) < 4 {
				continue
			}
			got++
			line, _ := strconv.Atoi(p[0])
			if msg := judge(expectation{line, p[1], p[2], p[3]}); msg != "" {
				problems = append(problems, fmt.Sprintf("line %d: %s", line, msg))
			}
		}
		f.Close()
	}
	if got < checks && len(problems) == 0 {
		problems = append(problems, fmt.Sprintf("%d of %d checks were reached (the block stopped early?): %s", got, checks, strings.TrimSpace(stderr.String())))
	}
	return problems
}

func isExit(err error) bool {
	_, ok := err.(*exec.ExitError)
	return ok
}

type caught struct {
	ID       int64  `json:"id"`
	Status   int    `json:"status"`
	Verdict  string `json:"verdict"`
	Verdicts []struct {
		Result string `json:"result"`
		Reason string `json:"reason"`
	} `json:"verdicts"`
}

func judge(e expectation) string {
	if e.kind == "output" {
		want, _ := os.ReadFile(e.arg)
		have, _ := os.ReadFile(e.extra)
		w := strings.Split(string(want), "\n")
		h := strings.Split(strings.TrimRight(string(have), "\n"), "\n")
		if len(w) != len(h) {
			return fmt.Sprintf("output has %d lines, the text shows %d:\n%s", len(h), len(w), have)
		}
		for i := range w {
			if p, ok := strings.CutSuffix(w[i], "…"); ok && strings.HasPrefix(h[i], p) || h[i] == w[i] {
				continue
			}
			return fmt.Sprintf("output line %d is %q, the text shows %q", i+1, h[i], w[i])
		}
		return ""
	}
	var r caught
	if err := json.Unmarshal([]byte(e.extra), &r); err != nil || r.ID == 0 {
		return "no request was caught"
	}
	switch e.kind {
	case "status":
		if strconv.Itoa(r.Status) != e.arg {
			return fmt.Sprintf("request %d was answered %d, the text says %s", r.ID, r.Status, e.arg)
		}
		return ""
	case "pass":
		if r.Verdict != "pass" {
			return fmt.Sprintf("request %d: verdict %q, the text says pass (%s)", r.ID, r.Verdict, e.extra)
		}
		return ""
	}
	for _, v := range r.Verdicts {
		if v.Result == e.kind && strings.Contains(v.Reason, e.arg) {
			return ""
		}
	}
	return fmt.Sprintf("request %d: no verdict %s containing %q in %s", r.ID, e.kind, e.arg, e.extra)
}
