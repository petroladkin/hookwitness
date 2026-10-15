package payload

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func render(toks []Token) string {
	var sb strings.Builder
	for _, t := range toks {
		sb.WriteString(t.Text)
	}
	return sb.String()
}

func TestPrettyJSON(t *testing.T) {
	toks, err := Pretty([]byte(`{"z":1,"a":{"n":1.50,"e":[],"o":{}},"l":[true,null,"x<y&z"],"u":"é"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "z": 1,
  "a": {
    "n": 1.50,
    "e": [],
    "o": {}
  },
  "l": [
    true,
    null,
    "x<y&z"
  ],
  "u": "é"
}`
	if got := render(toks); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	classes := map[string]string{}
	for _, tk := range toks {
		if tk.Class != "" {
			classes[tk.Text] = tk.Class
		}
	}
	if classes[`"z"`] != "k" || classes[`"x<y&z"`] != "s" || classes["1.50"] != "n" || classes["null"] != "n" {
		t.Fatalf("classes %v", classes)
	}
	if got, _ := Pretty([]byte(`[1,"a"]`)); render(got) != "[\n  1,\n  \"a\"\n]" {
		t.Fatalf("top-level array: %q", render(got))
	}
	if got, _ := Pretty([]byte(`"just a string"`)); render(got) != `"just a string"` {
		t.Fatalf("scalar: %q", render(got))
	}
}

func TestHexDump(t *testing.T) {
	got := HexDump([]byte("Hello, hookwitness\x00\xff"))
	want := "00000000  48 65 6c 6c 6f 2c 20 68  6f 6f 6b 77 69 74 6e 65  |Hello, hookwitne|\n" +
		"00000010  73 73 00 ff                                       |ss..|\n"
	if got != want {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
}

func TestParseQuery(t *testing.T) {
	got := fmt.Sprint(ParseQuery("b=2&a=1&b=%203&flag&bad=%zz&&e="))
	if got != "[{b 2} {a 1} {b  3} {flag } {bad %zz} {e }]" {
		t.Fatalf("got %s", got)
	}
}

func TestPrettyJSONPaths(t *testing.T) {
	toks, err := Pretty([]byte(`{"a":{"b":[1,{"c~/d":true}]},"e":null}`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tk := range toks {
		if tk.HasPath {
			got[tk.Text] += tk.Path + " "
		}
	}
	want := map[string]string{
		`"a"`: "/a ", `"b"`: "/a/b ", `1`: "/a/b/0 ", `"c~/d"`: "/a/b/1/c~0~1d ", `true`: "/a/b/1/c~0~1d ",
		`"e"`: "/e ", `null`: "/e ",
		`{`: " /a /a/b/1 ", `}`: "/a/b/1 /a  ", `[`: "/a/b ", `]`: "/a/b ",
	}
	for text, paths := range want {
		if got[text] != paths {
			t.Errorf("%s at %q, want %q", text, got[text], paths)
		}
	}
	MarkBad(toks, map[string]bool{"/a/b/0": true})
	for _, tk := range toks {
		if tk.Bad != (tk.Text == "1") {
			t.Errorf("Token %q bad=%v", tk.Text, tk.Bad)
		}
	}
}

// A deeply nested body stays small in the pretty view: indentation and
// paths stop growing (a 20 KB body used to become 190 MB of tokens).
func TestPrettyDeep(t *testing.T) {
	const depth = 9999
	body := []byte(strings.Repeat("[", depth) + "1" + strings.Repeat("]", depth))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	toks, err := Pretty(body)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	size := 0
	for _, tk := range toks {
		size += len(tk.Text) + len(tk.Path)
	}
	if size > 2<<20 {
		t.Fatalf("%d bytes of tokens", size)
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 32<<20 {
		t.Fatalf("allocated %d MB", n>>20)
	}
	// Shallow levels keep their paths; deep ones have none.
	if !toks[2].HasPath || toks[2].Path != "/0" {
		t.Fatalf("level 1: %+v", toks[2])
	}
	for _, tk := range toks {
		if tk.Text == "1" && tk.HasPath {
			t.Fatalf("a token at depth %d has a path", depth)
		}
	}
}
