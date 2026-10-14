package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Draft is a JSON Schema (2020-12) drafted from one or more JSON bodies, for
// a person to review: the types seen, objects with their properties (a key
// present in every sample required, others optional, additional ones
// allowed), arrays with one items schema merged from all their elements,
// integer when every number is whole, null joining the type list, and a
// string format only when every string there has it. Enums, patterns,
// ranges and lengths are never inferred. The draft validates every body it
// was drafted from.
func Draft(bodies [][]byte) (string, error) {
	if len(bodies) == 0 {
		return "", errors.New("no bodies to draft from")
	}
	root := &node{}
	for i, b := range bodies {
		if msg := BodyError(b); msg != "" {
			if len(bodies) == 1 {
				return "", errors.New(msg)
			}
			return "", fmt.Errorf("body %d: %s", i+1, msg)
		}
		v, err := parse(b)
		if err != nil {
			return "", err
		}
		root.add(v)
	}
	text := render(root, true)
	// A format this package thinks strict may still be refused by the
	// validator's own check; then the draft goes without formats.
	if !validatesAll(text, bodies) {
		text = render(root, false)
	}
	return text, nil
}

func validatesAll(text string, bodies [][]byte) bool {
	set, err := Compile([]Doc{{Name: "Draft", Text: text}})
	if err != nil {
		return false
	}
	for _, b := range bodies {
		if !set.Validate("Draft", b).OK {
			return false
		}
	}
	return true
}

// A JSON value with object keys in document order.
type (
	object []member
	member struct {
		key   string
		value any
	}
)

func parse(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var read func() (any, error)
	read = func() (any, error) {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				obj := object{}
				for dec.More() {
					k, err := dec.Token()
					if err != nil {
						return nil, err
					}
					v, err := read()
					if err != nil {
						return nil, err
					}
					obj = append(obj, member{k.(string), v})
				}
				_, err := dec.Token()
				return obj, err
			case '[':
				arr := []any{}
				for dec.More() {
					v, err := read()
					if err != nil {
						return nil, err
					}
					arr = append(arr, v)
				}
				_, err := dec.Token()
				return arr, err
			}
		}
		return tok, nil
	}
	return read()
}

// node is what the samples at one place in the bodies had in common.
type node struct {
	null, boolean, integer, number, str, obj, arr bool

	formats []string // the formats every string so far had; nil before the first

	props   []string // object keys in the order first seen
	byKey   map[string]*node
	present map[string]int // how many object samples had the key
	objects int            // object samples seen

	items *node // array elements, all of them; nil while no element was seen
}

func (n *node) add(v any) {
	switch t := v.(type) {
	case nil:
		n.null = true
	case bool:
		n.boolean = true
	case json.Number:
		if whole(t) {
			n.integer = true
		} else {
			n.number = true
		}
	case string:
		fs := formatsOf(t)
		if !n.str {
			n.formats = fs
		} else {
			n.formats = intersect(n.formats, fs)
		}
		n.str = true
	case object:
		n.obj = true
		n.objects++
		if n.byKey == nil {
			n.byKey, n.present = map[string]*node{}, map[string]int{}
		}
		seen := map[string]bool{}
		for _, m := range t {
			if seen[m.key] {
				continue // a repeated key: the last one wins in JSON, but the first is enough here
			}
			seen[m.key] = true
			c := n.byKey[m.key]
			if c == nil {
				c = &node{}
				n.byKey[m.key] = c
				n.props = append(n.props, m.key)
			}
			c.add(m.value)
			n.present[m.key]++
		}
	case []any:
		n.arr = true
		for _, e := range t {
			if n.items == nil {
				n.items = &node{}
			}
			n.items.add(e)
		}
	}
}

func whole(n json.Number) bool {
	f, ok := new(big.Float).SetString(string(n))
	return ok && f.IsInt()
}

// The formats a draft may claim, in the order one is preferred.
var (
	uuidRule = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	dateRule = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	formats  = []struct {
		name string
		is   func(string) bool
	}{
		{"date-time", func(s string) bool {
			_, err := time.Parse(time.RFC3339Nano, s)
			return err == nil && strings.ContainsAny(s[min(len(s), 10):], "Tt")
		}},
		{"date", func(s string) bool {
			_, err := time.Parse("2006-01-02", s)
			return err == nil && dateRule.MatchString(s)
		}},
		{"uuid", uuidRule.MatchString},
		{"email", func(s string) bool {
			a, err := mail.ParseAddress(s)
			return err == nil && a.Name == "" && a.Address == s
		}},
		{"uri", func(s string) bool {
			u, err := url.Parse(s)
			return err == nil && u.Scheme != "" && (u.Host != "" || u.Opaque != "") && !strings.ContainsAny(s, " \t\n")
		}},
	}
)

func formatsOf(s string) []string {
	out := []string{}
	for _, f := range formats {
		if f.is(s) {
			out = append(out, f.name)
		}
	}
	return out
}

func intersect(a, b []string) []string {
	out := []string{}
	for _, x := range a {
		for _, y := range b {
			if x == y {
				out = append(out, x)
			}
		}
	}
	return out
}

// render writes n as a schema, keys in a fixed, readable order.
func render(n *node, withFormats bool) string {
	var b strings.Builder
	w := writer{b: &b}
	w.open()
	w.key(`"$schema"`)
	w.raw(`"https://json-schema.org/draft/2020-12/schema"`)
	n.write(&w, withFormats)
	w.close()
	return b.String() + "\n"
}

func (n *node) write(w *writer, withFormats bool) {
	var types []string
	for _, t := range []struct {
		on   bool
		name string
	}{{n.obj, "object"}, {n.arr, "array"}, {n.str, "string"}, {n.integer && !n.number, "integer"},
		{n.number, "number"}, {n.boolean, "boolean"}, {n.null, "null"}} {
		if t.on {
			types = append(types, t.name)
		}
	}
	if len(types) > 0 { // none: an element of only empty arrays, or nothing seen; anything goes
		w.key(`"type"`)
		if len(types) == 1 {
			w.str(types[0])
		} else {
			w.list(types)
		}
	}
	if n.str && withFormats && len(n.formats) > 0 {
		w.key(`"format"`)
		w.str(n.formats[0])
	}
	if n.obj {
		if len(n.props) > 0 {
			w.key(`"properties"`)
			w.open()
			for _, k := range n.props {
				w.key(quote(k))
				w.open()
				n.byKey[k].write(w, withFormats)
				w.close()
			}
			w.close()
		}
		var req []string
		for _, k := range n.props {
			if n.present[k] == n.objects {
				req = append(req, k)
			}
		}
		if len(req) > 0 {
			w.key(`"required"`)
			w.list(req)
		}
	}
	if n.arr && n.items != nil {
		w.key(`"items"`)
		w.open()
		n.items.write(w, withFormats)
		w.close()
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// writer writes indented JSON objects member by member.
type writer struct {
	b     *strings.Builder
	depth int
	first []bool
}

func (w *writer) open() {
	w.b.WriteString("{")
	w.depth++
	w.first = append(w.first, true)
}

func (w *writer) close() {
	empty := w.first[len(w.first)-1]
	w.first = w.first[:len(w.first)-1]
	w.depth--
	if !empty {
		w.b.WriteString("\n" + strings.Repeat("  ", w.depth))
	}
	w.b.WriteString("}")
}

func (w *writer) key(k string) {
	if !w.first[len(w.first)-1] {
		w.b.WriteString(",")
	}
	w.first[len(w.first)-1] = false
	w.b.WriteString("\n" + strings.Repeat("  ", w.depth) + k + ": ")
}

func (w *writer) raw(s string) { w.b.WriteString(s) }
func (w *writer) str(s string) { w.b.WriteString(quote(s)) }

func (w *writer) list(items []string) {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = quote(s)
	}
	w.b.WriteString("[" + strings.Join(q, ", ") + "]")
}
