// Package payload prepares a caught request's body and query for people:
// JSON pretty-printed with every token tagged (class and JSON pointer),
// a text test, a hex dump, the query split into pairs. The UI and the
// reports show bodies the same way through it.
package payload

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Token is a piece of pretty-printed JSON with its highlight class
// ("k" key, "s" string, "n" number/literal, "" punctuation and space) and,
// for keys, values and brackets, the JSON pointer of the value it belongs
// to (a key: its member's value), so schema problems can be marked.
type Token struct {
	Class, Text string
	Path        string
	HasPath     bool
	Bad         bool   // a schema check found a problem at Path
	Image       bool   // an image is at Path
	Elided      string // set when Text was shortened: what was left out
}

// IsText reports UTF-8 text without control characters other than
// whitespace. A preview cut inside a multi-byte character still counts.
func IsText(b []byte) bool {
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size <= 1 {
			return len(b) < utf8.UTFMax && !utf8.FullRune(b)
		}
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
		b = b[size:]
	}
	return true
}

// Pretty indents a valid JSON document two spaces per level, keeping key
// order and number literals as received, and tags each Token for colour.
func Pretty(b []byte) ([]Token, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out []Token
	emit := func(class, text string) { out = append(out, Token{Class: class, Text: text}) }
	emitAt := func(class, text, path string) {
		out = append(out, Token{Class: class, Text: text, Path: path, HasPath: true})
	}
	indent := func(depth int) { emit("", "\n"+strings.Repeat("  ", depth)) }

	var stack []jsonFrame
	for {
		t, err := dec.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if d, ok := t.(json.Delim); ok && (d == '}' || d == ']') {
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if f.count > 0 {
				indent(len(stack))
			}
			emitAt("", string(d), f.path)
			afterValue(stack)
			continue
		}
		// A value or a key is about to be written.
		if n := len(stack); n > 0 {
			f := &stack[n-1]
			if !f.object || f.key {
				if f.count > 0 {
					emit("", ",")
				}
				indent(n)
				f.count++
			}
		}
		if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].key {
			k, _ := t.(string)
			stack[n-1].member = stack[n-1].path + "/" + PointerToken(k)
			emitAt("k", quote(k), stack[n-1].member)
			emit("", ": ")
			stack[n-1].key = false
			continue
		}
		path := ""
		if n := len(stack); n > 0 {
			if f := stack[n-1]; f.object {
				path = f.member
			} else {
				path = f.path + "/" + strconv.Itoa(f.count-1)
			}
		}
		switch v := t.(type) {
		case json.Delim:
			emitAt("", string(v), path)
			stack = append(stack, jsonFrame{object: v == '{', key: v == '{', path: path})
			continue
		case string:
			emitAt("s", quote(v), path)
		case json.Number:
			emitAt("n", v.String(), path)
		case bool:
			emitAt("n", fmt.Sprint(v), path)
		case nil:
			emitAt("n", "null", path)
		}
		afterValue(stack)
	}
}

// jsonFrame is an open object or array while pretty-printing.
type jsonFrame struct {
	object bool
	count  int    // members written
	key    bool   // in an object: the next Token is a key
	path   string // JSON pointer of this object or array
	member string // in an object: the pointer of the member being written
}

// PointerToken escapes a key for a JSON pointer (RFC 6901).
func PointerToken(k string) string { return strings.NewReplacer("~", "~0", "/", "~1").Replace(k) }

// MarkBad flags the tokens at the given JSON pointers.
func MarkBad(toks []Token, paths map[string]bool) {
	for i := range toks {
		if toks[i].HasPath && paths[toks[i].Path] {
			toks[i].Bad = true
		}
	}
}

// MarkImages flags the tokens at the given JSON pointers as images.
func MarkImages(toks []Token, paths map[string]bool) {
	for i := range toks {
		if toks[i].HasPath && paths[toks[i].Path] {
			toks[i].Image = true
		}
	}
}

// ElideImages shortens the image strings among the tokens to their first
// characters, noting how much was left out: a picture's base64 would fill
// the body view.
func ElideImages(toks []Token, keep int) {
	for i, t := range toks {
		if t.Image && t.Class == "s" && len(t.Text) > keep+16 {
			toks[i].Elided = fmt.Sprintf(" %d more characters", len(t.Text)-keep-2)
			toks[i].Text = t.Text[:keep] + "…\""
		}
	}
}

// afterValue marks that the next Token in an enclosing object is a key.
func afterValue(stack []jsonFrame) {
	if n := len(stack); n > 0 && stack[n-1].object {
		stack[n-1].key = true
	}
}

func quote(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

// HexDump formats b as offset, 16 hex bytes and their printable ASCII.
func HexDump(b []byte) string {
	var sb strings.Builder
	for off := 0; off < len(b); off += 16 {
		line := b[off:min(off+16, len(b))]
		fmt.Fprintf(&sb, "%08x  ", off)
		for i := range 16 {
			if i < len(line) {
				fmt.Fprintf(&sb, "%02x ", line[i])
			} else {
				sb.WriteString("   ")
			}
			if i == 7 {
				sb.WriteByte(' ')
			}
		}
		sb.WriteString(" |")
		for _, c := range line {
			if c >= 0x20 && c < 0x7f {
				sb.WriteByte(c)
			} else {
				sb.WriteByte('.')
			}
		}
		sb.WriteString("|\n")
	}
	return sb.String()
}

// QueryParam is one name=value pair of a query string, in received order.
type QueryParam struct {
	Name, Value string
}

// ParseQuery splits a raw query into decoded pairs, keeping order and
// repeats; a part that does not decode is shown as written.
func ParseQuery(raw string) []QueryParam {
	var out []QueryParam
	for part := range strings.SplitSeq(raw, "&") {
		if part == "" {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		out = append(out, QueryParam{unescape(name), unescape(value)})
	}
	return out
}

func unescape(s string) string {
	if u, err := url.QueryUnescape(s); err == nil {
		return u
	}
	return s
}
