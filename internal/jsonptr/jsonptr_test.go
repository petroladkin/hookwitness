package jsonptr

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The example document of RFC 6901, section 5.
const rfcDoc = `{
  "foo": ["bar", "baz"],
  "": 0,
  "a/b": 1,
  "c%d": 2,
  "e^f": 3,
  "g|h": 4,
  "i\\j": 5,
  "k\"l": 6,
  " ": 7,
  "m~n": 8
}`

func decode(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRFC6901Examples(t *testing.T) {
	doc := decode(t, rfcDoc)
	for ptr, want := range map[string]string{
		"/foo/0": `"bar"`, "/": "0", "/a~1b": "1", "/c%d": "2", "/e^f": "3", "/g|h": "4",
		`/i\j`: "5", `/k"l`: "6", "/ ": "7", "/m~0n": "8", "/foo": `["bar","baz"]`,
	} {
		toks, err := Parse(ptr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", ptr, err)
		}
		v, ok := Get(doc, toks)
		got, _ := json.Marshal(v)
		if !ok || string(got) != want {
			t.Errorf("Get(%q) = %s %v, want %s", ptr, got, ok, want)
		}
	}
	if v, ok := Get(doc, nil); !ok || !reflect.DeepEqual(v, doc) {
		t.Error(`"" is not the whole document`)
	}
}

// Array indexes are "0" or digits without a leading zero: no sign, no
// leading zero, nothing past the end ("-" names the element after it).
func TestIndex(t *testing.T) {
	doc := decode(t, `{"items":["a","b","c","d","e","f","g","h","i","j","k"]}`)
	for _, tok := range []string{"01", "+1", "-0", "-1", "1.0", " 1", "1 ", "", "-", "11", "99999999999999999999", "0x1", "1e0"} {
		if v, ok := Get(doc, []string{"items", tok}); ok {
			t.Errorf("index %q matched %v", tok, v)
		}
	}
	for tok, want := range map[string]string{"0": "a", "1": "b", "10": "k"} {
		if v, ok := Get(doc, []string{"items", tok}); !ok || v != want {
			t.Errorf("index %q = %v %v, want %s", tok, v, ok, want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, ptr := range []string{"foo", "/a~", "/a~2", "/~x"} {
		if _, err := Parse(ptr); err == nil {
			t.Errorf("Parse(%q) accepted", ptr)
		}
	}
	if toks, err := Parse(""); err != nil || toks != nil {
		t.Errorf(`Parse("") = %v %v`, toks, err)
	}
	if toks, _ := Parse("/a~01"); !reflect.DeepEqual(toks, []string{"a~1"}) {
		t.Errorf("~01 is ~ then 1: %q", toks)
	}
}

func TestFormat(t *testing.T) {
	toks := []string{"a/b", "m~n", "0", ""}
	ptr := Format(toks)
	if ptr != "/a~1b/m~0n/0/" {
		t.Fatalf("Format = %q", ptr)
	}
	back, err := Parse(ptr)
	if err != nil || !reflect.DeepEqual(back, toks) {
		t.Fatalf("round trip: %q %v", back, err)
	}
	if Escape("~/") != "~0~1" {
		t.Fatal(Escape("~/"))
	}
}

// Get walks only objects and arrays.
func TestGetThroughScalars(t *testing.T) {
	doc := decode(t, `{"a":"text","b":null,"c":{"d":true}}`)
	for _, ptr := range []string{"/a/0", "/b/x", "/c/d/e", "/x"} {
		toks, _ := Parse(ptr)
		if v, ok := Get(doc, toks); ok {
			t.Errorf("Get(%q) = %v", ptr, v)
		}
	}
	if v, ok := Get(doc, []string{"b"}); !ok || v != nil {
		t.Errorf("a null value is found: %v %v", v, ok)
	}
}
