// Package jsonptr resolves JSON pointers (RFC 6901) in documents decoded by
// encoding/json: one resolver for every place Hookwitness reads a value at
// a pointer (a schema chosen by a body field, problem paths, the json=
// filter, an image in a body).
package jsonptr

import (
	"errors"
	"strings"
)

var (
	unescape = strings.NewReplacer("~1", "/", "~0", "~")
	escape   = strings.NewReplacer("~", "~0", "/", "~1")
)

// Parse splits ptr into its reference tokens, unescaped. "" is the whole
// document (no tokens); any other pointer starts with "/", and "~" is
// followed only by 0 or 1.
func Parse(ptr string) ([]string, error) {
	if ptr == "" {
		return nil, nil
	}
	if ptr[0] != '/' {
		return nil, errors.New("a JSON pointer starts with /")
	}
	toks := strings.Split(ptr[1:], "/")
	for i, t := range toks {
		for j := 0; j < len(t); j++ {
			if t[j] == '~' && (j+1 == len(t) || t[j+1] != '0' && t[j+1] != '1') {
				return nil, errors.New("~ in a JSON pointer is followed by 0 or 1")
			}
		}
		toks[i] = unescape.Replace(t)
	}
	return toks, nil
}

// Escape is one token as written in a pointer.
func Escape(tok string) string { return escape.Replace(tok) }

// Format is the pointer of tokens.
func Format(tokens []string) string {
	var sb strings.Builder
	for _, t := range tokens {
		sb.WriteByte('/')
		sb.WriteString(escape.Replace(t))
	}
	return sb.String()
}

// Index reads tok as an index into an array of n elements: "0" or digits
// without a leading zero (no sign), below n.
func Index(tok string, n int) (int, bool) {
	if tok == "" || len(tok) > 1 && tok[0] == '0' {
		return 0, false
	}
	i := 0
	for _, c := range []byte(tok) {
		if c < '0' || c > '9' {
			return 0, false
		}
		i = i*10 + int(c-'0')
		if i >= n {
			return 0, false
		}
	}
	return i, true
}

// Get is the value at tokens in doc (objects as map[string]any, arrays as
// []any), and whether there is one.
func Get(doc any, tokens []string) (any, bool) {
	cur := doc
	for _, tok := range tokens {
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[tok]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, ok := Index(tok, len(v))
			if !ok {
				return nil, false
			}
			cur = v[i]
		default:
			return nil, false
		}
	}
	return cur, true
}
