package report

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/emulate"
)

// setupFixture is an endpoint's behaviour with every part a setup section
// shows, and secrets with synthetic seals, hashes and fingerprints.
func setupFixture(t testing.TB) *emulate.Config {
	t.Helper()
	b, err := os.ReadFile("../../testdata/report/setup.json")
	if err != nil {
		t.Fatal(err)
	}
	var c emulate.Config
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

func setupOptions(t testing.TB) Options {
	o := goldenOptions()
	o.Setup = setupFixture(t)
	return o
}

// What a report must never carry of a secret: its seal, its hash, its
// fingerprint, and the parts they are made of.
var setupSecrets = []string{
	"SEALED-TEST-ONLY", "a1b2c3d4e5f60718293a4b5c6d7e8f90", "v1.SEALED",
	"HASH-TEST-ONLY", "SALT-TEST-ONLY", "pbkdf2-sha256$",
	"fp-test-only", "9e8d7c6b", "1a2b3c4d",
}

func TestSetupGolden(t *testing.T) {
	rs := fixture(t)[:1]
	for _, f := range []Format{Markdown, HTML} {
		t.Run(string(f), func(t *testing.T) {
			got := render(t, f, setupOptions(t), rs)
			path := "../../testdata/report/setup." + string(f)
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("%s differs from %s; run with -update after an intended change and review the diff", f, path)
			}
		})
	}
}

// AC-1: the section says how the endpoint was set up, part by part, in
// order.
func TestSetupSection(t *testing.T) {
	md := render(t, Markdown, setupOptions(t), fixture(t)[:1])
	_, section, ok := strings.Cut(md, "## Setup\n")
	if !ok {
		t.Fatal("no setup section")
	}
	section, _, _ = strings.Cut(section, "## Requests")
	in := func(want ...string) {
		t.Helper()
		at := 0
		for _, w := range want {
			i := strings.Index(section[at:], w)
			if i < 0 {
				t.Fatalf("lacks %q (in this order) in:\n%s", w, section)
			}
			at += i + len(w)
		}
	}
	in("**Checks**",
		"| 1 | header X-Shop-Domain | equals \"test-shop.example\" · else 400 naming the header |",
		"| 2 | GitHub signature | secret GITHUB\\_KEY · enforced · else 401 |",
		"| 3 | Bearer token | secret API\\_TOKEN · record only |",
		"| 4 | JSON Schema by /type | \"order.created\" → order · other values → event · enforced · else 400 listing the problems |")
	in("**Schemas**", "| order | check 4 (JSON Schema by /type) |", "| event | check 4", "| draft | not used |")
	in("**Rules**",
		"| 1 | every 3rd | 503 after 2.0 s+ |", "| 2 | first 1 | time out |", "| 3 | 10% | drop the connection |", "| 4 | every 7th | 202 |",
		"Rule 1 · every 3rd · 503", "| Delay | 2.0 s + up to 500 ms at random |", "| `Retry-After` | `2` |", "busy | try `later`",
		"Rule 4 · every 7th · 202", "4 B given as exact bytes")
	in("**Default reply**", "| Status | 200 |", "| Delay | none |", "| `Content-Type` | `application/json` |", `{"ok":true,"shop":"{{.Var "shop"}}"}`)
	in("**Variables**", "| `shop` | `test-shop` |")
	in("**Secrets**",
		"| GITHUB\\_KEY | signing key | set | check 2 (GitHub signature) |",
		"| API\\_TOKEN | token or password | not set | check 3 (Bearer token) |",
		"| SPARE\\_TOKEN | token or password | set | not used |")
}

// AC-2: the section names the time it shows, the generation time.
func TestSetupTime(t *testing.T) {
	for _, f := range []Format{Markdown, HTML} {
		out := render(t, f, setupOptions(t), fixture(t)[:1])
		if !regexp.MustCompile(`(?s)Setup.{0,200}when this report was generated.{0,40}2026-10-12T09:30:00\.000Z`).MatchString(out) {
			t.Fatalf("%s: the section does not say it is the setup at the generation time", f)
		}
	}
	// Only the lines that name the generation time change with it.
	rs := fixture(t)
	for _, f := range []Format{Markdown, HTML} {
		a := render(t, f, setupOptions(t), rs)
		if b := render(t, f, setupOptions(t), rs); a != b {
			t.Fatalf("%s: two runs differ", f)
		}
		o := setupOptions(t)
		o.Generated = o.Generated.Add(time.Hour)
		al, bl := strings.Split(a, "\n"), strings.Split(render(t, f, o, rs), "\n")
		if len(al) != len(bl) {
			t.Fatalf("%s: %d lines vs %d", f, len(al), len(bl))
		}
		for i := range al {
			if al[i] != bl[i] && !strings.Contains(al[i], "2026-10-12T09:30:00.000Z") {
				t.Fatalf("%s line %d differs:\n%s\n%s", f, i+1, al[i], bl[i])
			}
		}
	}
}

// AC-3: no secret value, hash, seal or fingerprint in any format, the PDF
// read by the text it draws and by its inflated streams.
func TestSetupNoSecrets(t *testing.T) {
	rs := fixture(t)
	for _, f := range []Format{Markdown, HTML} {
		out := render(t, f, setupOptions(t), rs)
		for _, s := range setupSecrets {
			if strings.Contains(out, s) {
				t.Fatalf("%s: %q is in the report", f, s)
			}
		}
	}
	var drawn strings.Builder
	pdfTextHook = func(s string) { drawn.WriteString(s + "\n") }
	defer func() { pdfTextHook = nil }()
	pdf := render(t, PDF, setupOptions(t), rs)
	text := drawn.String()
	for _, want := range []string{"SETUP", "GITHUB_KEY", "not set", "every 3rd", "test-shop"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the PDF does not draw %q", want)
		}
	}
	all := pdf + inflated(t, []byte(pdf))
	for _, s := range setupSecrets {
		if strings.Contains(text, s) || strings.Contains(all, s) {
			t.Fatalf("pdf: %q is in the report", s)
		}
	}
}

// inflated is the PDF's compressed streams, decompressed.
func inflated(t testing.TB, pdf []byte) string {
	var sb strings.Builder
	for _, m := range regexp.MustCompile(`(?s)stream\r?\n(.*?)endstream`).FindAllSubmatch(pdf, -1) {
		r, err := zlib.NewReader(bytes.NewReader(m[1]))
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(r)
		sb.Write(b)
	}
	return sb.String()
}

// AC-4: off by default; the Unmatched register has no setup to show.
func TestSetupOff(t *testing.T) {
	rs := fixture(t)[:1]
	for _, f := range []Format{Markdown, HTML} {
		if out := render(t, f, goldenOptions(), rs); strings.Contains(out, "Setup") {
			t.Fatalf("%s: a setup section without asking", f)
		}
		o := setupOptions(t)
		o.Endpoint = ""
		if out := render(t, f, o, rs); strings.Contains(out, "Setup") {
			t.Fatalf("%s: the Unmatched register's report has a setup section", f)
		}
	}
}

// An endpoint with nothing set up beyond the default reply says so.
func TestSetupEmpty(t *testing.T) {
	o := goldenOptions()
	c := emulate.DefaultConfig()
	o.Setup = &c
	md := render(t, Markdown, o, nil)
	for _, want := range []string{"**Checks** (in order)\n\nNone.", "**Rules** (in order; the first that holds answers)\n\nNone: every request gets the default reply.", "**Variables**\n\nNone.", "**Secrets** (named, never shown)\n\nNone.", "| Status | 200 |", "| Body | empty |"} {
		if !strings.Contains(md, want) {
			t.Fatalf("lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "**Schemas**") {
		t.Fatal("a schemas part with no schemas")
	}
}
