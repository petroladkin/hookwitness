package emulate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/schema"
)

// Schema is a JSON Schema stored with an endpoint, referred to by name.
type Schema = schema.Doc

// Where a schema check reads the value that picks the schema.
const (
	FromBody   = "body"   // a JSON pointer into the body, e.g. /type
	FromHeader = "header" // a header, e.g. X-GitHub-Event
)

// Select picks a schema by a value of the request: a discriminator.
type Select struct {
	From string    `json:"from"` // FromBody or FromHeader
	Key  string    `json:"key"`  // the JSON pointer or the header name
	Map  []Mapping `json:"map"`  // in order; the first equal value wins
}

// Mapping maps one discriminator value to a schema.
type Mapping struct {
	Value  string `json:"value"`
	Schema string `json:"schema"`
}

// Label names the discriminator: "/type" or "header X-GitHub-Event".
func (s Select) Label() string {
	if s.From == FromHeader {
		return "header " + s.Key
	}
	return s.Key
}

// compiled is the endpoint's schemas compiled (and remembered).
func (c Config) compiled() (*schema.Set, error) { return schema.Cached(c.Schemas) }

func (c Config) validateSchemaCheck(ch Check, maxWait time.Duration) error {
	if len(c.Schemas) == 0 {
		return errors.New("add a schema first (Schemas tab)")
	}
	set, err := c.compiled()
	if err != nil {
		return err
	}
	known := func(name string) error {
		if !set.Has(name) {
			return fmt.Errorf("there is no schema named %q", name)
		}
		return nil
	}
	if s := ch.Select; s != nil {
		switch s.From {
		case FromBody:
			if !strings.HasPrefix(s.Key, "/") {
				return fmt.Errorf("the value to pick the schema by is a JSON pointer such as /type, not %q", s.Key)
			}
		case FromHeader:
			if !headerName.MatchString(s.Key) {
				return fmt.Errorf("header name %q is not a valid HTTP header name", s.Key)
			}
		default:
			return fmt.Errorf("pick the schema by the body or a header, not %q", s.From)
		}
		if len(s.Map) == 0 {
			return errors.New("map at least one value to a schema")
		}
		seen := map[string]bool{}
		for _, m := range s.Map {
			if seen[m.Value] {
				return fmt.Errorf("the value %q is mapped twice", m.Value)
			}
			seen[m.Value] = true
			if err := known(m.Schema); err != nil {
				return err
			}
		}
	} else if ch.Schema == "" {
		return errors.New("choose the schema")
	} else if err := known(ch.Schema); err != nil {
		return err
	}
	if ch.Error != nil {
		if err := ch.Error.validate(maxWait); err != nil {
			return fmt.Errorf("error reply: %w", err)
		}
	}
	return nil
}

// verifySchema gives a schema check's verdict.
func (c Config) verifySchema(ch Check, r Request, hasBody bool) auth.Verdict {
	if !hasBody {
		return auth.Verdict{Result: auth.NotVerifiable, Reason: "body not kept (over the size limit)"}
	}
	set, err := c.compiled()
	if err != nil {
		return auth.Verdict{Result: auth.NotVerifiable, Reason: err.Error()}
	}
	name := ch.Schema
	if s := ch.Select; s != nil {
		value, found, err := s.value(r)
		if err != nil {
			// The body is not JSON: that is the verdict, whatever the schema.
			return auth.Verdict{Result: auth.Fail, Reason: "not JSON", Problems: []auth.Problem{{Keyword: "json", Message: err.Error()}}, Total: 1}
		}
		if !found {
			return auth.Verdict{Result: auth.NotChecked, Reason: "no " + s.Label() + " in the request"}
		}
		name = ""
		for _, m := range s.Map {
			if m.Value == value {
				name = m.Schema
				break
			}
		}
		if name == "" {
			return auth.Verdict{Result: auth.NotChecked, Reason: fmt.Sprintf("no schema for %s %q", s.Label(), value)}
		}
	}
	if !set.Has(name) {
		return auth.Verdict{Result: auth.NotVerifiable, Reason: "schema " + name + " does not exist"}
	}
	res := set.Validate(name, r.Body)
	if res.OK {
		return auth.Verdict{Result: auth.Pass, Reason: "matches " + name}
	}
	v := auth.Verdict{Result: auth.Fail, Problems: res.Problems, Total: res.Total}
	switch {
	case res.Total == 1 && res.Problems[0].Keyword == "json":
		v.Reason = res.Problems[0].Message
	case res.Total == 1:
		v.Reason = "1 error against " + name
	default:
		v.Reason = strconv.Itoa(res.Total) + " errors against " + name
	}
	return v
}

// value reads the discriminator from the request. An error means the body
// is not JSON (when the value comes from the body).
func (s Select) value(r Request) (value string, found bool, err error) {
	if s.From == FromHeader {
		vs := headerValues(r.Headers, s.Key)
		if len(vs) == 0 {
			return "", false, nil
		}
		return vs[0], true, nil
	}
	dec := json.NewDecoder(bytes.NewReader(r.Body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		if len(bytes.TrimSpace(r.Body)) == 0 {
			return "", false, errors.New("empty body")
		}
		return "", false, fmt.Errorf("not JSON: %v", err)
	}
	cur := doc
	for _, tok := range strings.Split(s.Key[1:], "/") {
		tok = strings.NewReplacer("~1", "/", "~0", "~").Replace(tok)
		switch v := cur.(type) {
		case map[string]any:
			if cur, found = v[tok]; !found {
				return "", false, nil
			}
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(v) {
				return "", false, nil
			}
			cur = v[i]
		default:
			return "", false, nil
		}
	}
	switch v := cur.(type) {
	case string:
		return v, true, nil
	case json.Number:
		return v.String(), true, nil
	case bool:
		return strconv.FormatBool(v), true, nil
	}
	return "", false, nil // an object, array or null picks nothing
}

// schemaRefusal is the standard reply to a body refused by a schema check:
// 400 with the problems found, one per line.
func (f Failed) schemaRefusal() Reply {
	var b strings.Builder
	b.WriteString("Request refused: " + f.Check.Label() + ": " + f.Why + ".\n")
	for _, p := range f.Problems {
		where := p.Path
		if where == "" {
			where = "(body)"
		}
		b.WriteString(where + ": " + p.Message + "\n")
	}
	if f.Total > len(f.Problems) {
		fmt.Fprintf(&b, "... and %d more\n", f.Total-len(f.Problems))
	}
	return Reply{
		Status:  400,
		Headers: []Header{{Name: "Content-Type", Value: "text/plain; charset=utf-8"}},
		Body:    literal(b.String()),
	}
}
