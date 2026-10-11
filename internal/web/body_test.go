package web

import (
	"fmt"
	"strings"
	"testing"
)

func render(toks []token) string {
	var sb strings.Builder
	for _, t := range toks {
		sb.WriteString(t.Text)
	}
	return sb.String()
}

func TestPrettyJSON(t *testing.T) {
	toks, err := prettyJSON([]byte(`{"z":1,"a":{"n":1.50,"e":[],"o":{}},"l":[true,null,"x<y&z"],"u":"é"}`))
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
	if got, _ := prettyJSON([]byte(`[1,"a"]`)); render(got) != "[\n  1,\n  \"a\"\n]" {
		t.Fatalf("top-level array: %q", render(got))
	}
	if got, _ := prettyJSON([]byte(`"just a string"`)); render(got) != `"just a string"` {
		t.Fatalf("scalar: %q", render(got))
	}
}

func TestViewBody(t *testing.T) {
	cases := []struct {
		body string
		kind bodyKind
	}{
		{"", bodyEmpty},
		{`{"a":1}`, bodyJSON},
		{"a=1&b=2", bodyText},
		{"привіт, світ\n", bodyText},
		{"<xml/>", bodyText},
		{"\x00\x01binary", bodyBinary},
		{"\xff\xfe", bodyBinary},
		{`{"a":`, bodyText}, // broken JSON is still text
	}
	for _, c := range cases {
		if got := viewBody([]byte(c.body)).Kind; got != c.kind {
			t.Errorf("%q: kind %d, want %d", c.body, got, c.kind)
		}
	}
	big := viewBody([]byte(strings.Repeat("a", previewLimit+10)))
	if big.Kind != bodyText || !big.Truncated || len(big.Text) != previewLimit {
		t.Errorf("large text: kind %d truncated %v len %d", big.Kind, big.Truncated, len(big.Text))
	}
	// Cut inside a multi-byte character: still text.
	cut := viewBody([]byte(strings.Repeat("a", previewLimit-1) + "é"))
	if cut.Kind != bodyText {
		t.Errorf("text cut in a character became kind %d", cut.Kind)
	}
	bin := viewBody(make([]byte, 2000))
	if bin.Kind != bodyBinary || !bin.Truncated || strings.Count(bin.Hex, "\n") != hexLimit/16 {
		t.Errorf("binary preview: %d lines, truncated %v", strings.Count(bin.Hex, "\n"), bin.Truncated)
	}
}

func TestHexDump(t *testing.T) {
	got := hexDump([]byte("Hello, hookwitness\x00\xff"))
	want := "00000000  48 65 6c 6c 6f 2c 20 68  6f 6f 6b 77 69 74 6e 65  |Hello, hookwitne|\n" +
		"00000010  73 73 00 ff                                       |ss..|\n"
	if got != want {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
}

func TestParseQuery(t *testing.T) {
	got := fmt.Sprint(parseQuery("b=2&a=1&b=%203&flag&bad=%zz&&e="))
	if got != "[{b 2} {a 1} {b  3} {flag } {bad %zz} {e }]" {
		t.Fatalf("got %s", got)
	}
}

func TestSize(t *testing.T) {
	for n, want := range map[int64]string{-1: "unknown size", 0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 10 << 20: "10.0 MiB"} {
		if got := size(n); got != want {
			t.Errorf("size(%d) = %q, want %q", n, got, want)
		}
	}
}
