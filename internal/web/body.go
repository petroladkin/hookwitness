package web

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

// previewLimit bounds how much of a body is rendered on the page; the
// download always has all of it.
const previewLimit = 256 << 10

// hexLimit bounds the hex preview of a binary body.
const hexLimit = 512

// bodyKind is how a body is shown.
type bodyKind int

const (
	bodyEmpty bodyKind = iota
	bodyJSON
	bodyText
	bodyBinary
)

// token is a piece of pretty-printed JSON with its highlight class
// ("k" key, "s" string, "n" number/literal, "" punctuation and space) and,
// for keys, values and brackets, the JSON pointer of the value it belongs
// to (a key: its member's value), so schema problems can be marked.
type token struct {
	Class, Text string
	Path        string
	HasPath     bool
	Bad         bool // a schema check found a problem at Path
}

// bodyView is a body prepared for the page.
type bodyView struct {
	Kind      bodyKind
	JSON      []token // bodyJSON
	Text      string  // bodyText; the raw view of bodyJSON
	Hex       string  // bodyBinary
	Truncated bool    // only the first previewLimit (hexLimit) bytes are shown
}

func viewBody(b []byte) bodyView {
	switch {
	case len(b) == 0:
		return bodyView{Kind: bodyEmpty}
	case len(b) <= previewLimit && json.Valid(b):
		if toks, err := prettyJSON(b); err == nil {
			return bodyView{Kind: bodyJSON, JSON: toks, Text: string(b)}
		}
	}
	shown, cut := b, false
	if len(shown) > previewLimit {
		shown, cut = shown[:previewLimit], true
	}
	if isText(shown) {
		return bodyView{Kind: bodyText, Text: string(shown), Truncated: cut}
	}
	n := min(len(b), hexLimit)
	return bodyView{Kind: bodyBinary, Hex: hexDump(b[:n]), Truncated: len(b) > n}
}

// isText reports UTF-8 text without control characters other than
// whitespace. A preview cut inside a multi-byte character still counts.
func isText(b []byte) bool {
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

// prettyJSON indents a valid JSON document two spaces per level, keeping key
// order and number literals as received, and tags each token for colour.
func prettyJSON(b []byte) ([]token, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out []token
	emit := func(class, text string) { out = append(out, token{Class: class, Text: text}) }
	emitAt := func(class, text, path string) {
		out = append(out, token{Class: class, Text: text, Path: path, HasPath: true})
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
			stack[n-1].member = stack[n-1].path + "/" + pointerToken(k)
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
	key    bool   // in an object: the next token is a key
	path   string // JSON pointer of this object or array
	member string // in an object: the pointer of the member being written
}

// pointerToken escapes a key for a JSON pointer (RFC 6901).
func pointerToken(k string) string { return strings.NewReplacer("~", "~0", "/", "~1").Replace(k) }

// markBad flags the tokens at the given JSON pointers.
func markBad(toks []token, paths map[string]bool) {
	for i := range toks {
		if toks[i].HasPath && paths[toks[i].Path] {
			toks[i].Bad = true
		}
	}
}

// afterValue marks that the next token in an enclosing object is a key.
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

// hexDump formats b as offset, 16 hex bytes and their printable ASCII.
func hexDump(b []byte) string {
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

// queryParam is one name=value pair of a query string, in received order.
type queryParam struct {
	Name, Value string
}

// parseQuery splits a raw query into decoded pairs, keeping order and
// repeats; a part that does not decode is shown as written.
func parseQuery(raw string) []queryParam {
	var out []queryParam
	for part := range strings.SplitSeq(raw, "&") {
		if part == "" {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		out = append(out, queryParam{unescape(name), unescape(value)})
	}
	return out
}

func unescape(s string) string {
	if u, err := url.QueryUnescape(s); err == nil {
		return u
	}
	return s
}
