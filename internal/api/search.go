package api

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// stamp is a time in an answer: RFC 3339 in UTC with exactly three
// fraction digits, so that scripts can sort and slice it as text.
type stamp time.Time

const stampLayout = "2006-01-02T15:04:05.000Z07:00"

func (s stamp) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(s).UTC().Format(stampLayout) + `"`), nil
}

// UnmarshalJSON reads any RFC 3339 time (the tests decode answers).
func (s *stamp) UnmarshalJSON(b []byte) error {
	var t time.Time
	if err := t.UnmarshalJSON(b); err != nil {
		return err
	}
	*s = stamp(t)
	return nil
}

// jsonMatch is one json=<pointer>=<value> filter: the JSON body has, at the
// pointer, a string equal to the value, or a number, true, false or null
// written exactly as the value.
type jsonMatch struct {
	tokens []string // the pointer's reference tokens, unescaped
	value  string
}

// parseJSONMatch reads <pointer>=<value>; the pointer ends at the first =.
func parseJSONMatch(s string) (jsonMatch, bool) {
	ptr, value, ok := strings.Cut(s, "=")
	if !ok || !strings.HasPrefix(ptr, "/") {
		return jsonMatch{}, false
	}
	m := jsonMatch{value: value}
	for _, tok := range strings.Split(ptr[1:], "/") {
		m.tokens = append(m.tokens, strings.NewReplacer("~1", "/", "~0", "~").Replace(tok))
	}
	return m, true
}

// matchAll reports a body that is JSON and satisfies every match.
func matchAll(ms []jsonMatch) func(body []byte) bool {
	return func(body []byte) bool {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		var doc any
		if dec.Decode(&doc) != nil {
			return false
		}
		for _, m := range ms {
			if !m.matches(doc) {
				return false
			}
		}
		return true
	}
}

func (m jsonMatch) matches(doc any) bool {
	v := doc
	for _, tok := range m.tokens {
		switch c := v.(type) {
		case map[string]any:
			next, ok := c[tok]
			if !ok {
				return false
			}
			v = next
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(c) || (len(tok) > 1 && tok[0] == '0') {
				return false
			}
			v = c[i]
		default:
			return false
		}
	}
	switch x := v.(type) {
	case string:
		return x == m.value
	case json.Number:
		return string(x) == m.value
	case bool:
		return strconv.FormatBool(x) == m.value
	case nil:
		return m.value == "null"
	}
	return false // an object or an array
}
