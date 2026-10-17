package emulate

import (
	"strings"
	"unicode"
)

// DraftHint is what a schema drafted from some requests may be called and
// where it may be mapped: the first schema check that picks its schema by a
// discriminator, and the value the requests carry there.
type DraftHint struct {
	Name   string // suggested: the mapped schema, else from the value, else from the path
	Check  int    // 1-based index of that check; 0 when there is none
	Label  string // that check's label
	Value  string // the value all the requests share; "" when they differ or lack it
	Mapped string // the schema the value is already mapped to; "" when none
}

// DraftHint looks at the endpoint (its path) and the requests a schema is
// drafted from.
func (c Config) DraftHint(path string, reqs []Request) DraftHint {
	var h DraftHint
	for i, ch := range c.Checks {
		if ch.kind() != KindSchema || ch.Select == nil {
			continue
		}
		h.Check, h.Label = i+1, ch.Label()
		for j, r := range reqs {
			v, found, err := ch.Select.value(r)
			if err != nil || !found || (j > 0 && v != h.Value) {
				h.Value = ""
				break
			}
			h.Value = v
		}
		if h.Value != "" {
			for _, m := range ch.Select.Map {
				if m.Value == h.Value {
					h.Mapped = m.Schema
					break
				}
			}
		}
		break
	}
	switch {
	case h.Mapped != "":
		h.Name = h.Mapped
	case h.Value != "":
		h.Name = SchemaName(h.Value)
	default:
		seg := path[strings.LastIndex(strings.TrimRight(path, "/"), "/")+1:]
		h.Name = SchemaName(seg)
	}
	return h
}

// SchemaName turns a value or a path segment into a schema name:
// order.paid → OrderPaid, pull_request → PullRequest; "Payload" when
// nothing is left.
func SchemaName(s string) string {
	var b strings.Builder
	up := true
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) || r > unicode.MaxASCII {
			up = true
			continue
		}
		if up {
			r = unicode.ToUpper(r)
		}
		up = false
		b.WriteRune(r)
	}
	name := b.String()
	if name == "" {
		return "Payload"
	}
	if unicode.IsDigit(rune(name[0])) {
		name = "Schema" + name
	}
	return name
}

// MapValue adds value → schema to the mapping of check (1-based), unless
// the value is mapped already; it reports whether it added it.
func (c *Config) MapValue(check int, value, schema string) bool {
	if check < 1 || check > len(c.Checks) || c.Checks[check-1].Select == nil || value == "" {
		return false
	}
	s := c.Checks[check-1].Select
	for _, m := range s.Map {
		if m.Value == value {
			return false
		}
	}
	s.Map = append(s.Map, Mapping{Value: value, Schema: schema})
	return true
}
