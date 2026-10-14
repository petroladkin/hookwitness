package emulate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	// Default is checked when the value has no mapping or is missing; one
	// schema or several (any of). Without it such requests are not checked.
	Default Names `json:"default,omitempty"`
}

// Names is one schema name or several, the body to match any of them; in
// JSON "V1" or ["V1","V2"].
type Names []string

func (n *Names) UnmarshalJSON(b []byte) error {
	if bytes.HasPrefix(bytes.TrimSpace(b), []byte(`"`)) {
		var one string
		if err := json.Unmarshal(b, &one); err != nil {
			return err
		}
		*n = Names{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("a schema name or a list of them")
	}
	*n = many
	return nil
}

func (n Names) MarshalJSON() ([]byte, error) {
	if len(n) == 1 {
		return json.Marshal(n[0])
	}
	return json.Marshal([]string(n))
}

// alternatives are the schemas of a check without a Select: Schemas, or
// the one Schema.
func (ch Check) alternatives() []string {
	if len(ch.Schemas) > 0 {
		return ch.Schemas
	}
	return []string{ch.Schema}
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

// SchemaUsers names the checks that use a schema, for refusing its
// deletion or rename.
func (c Config) SchemaUsers(name string) []string {
	var out []string
	for i, ch := range c.Checks {
		if ch.kind() != KindSchema {
			continue
		}
		used := ch.Select == nil && slices.Contains(ch.alternatives(), name)
		if ch.Select != nil {
			used = slices.Contains(ch.Select.Default, name)
			for _, m := range ch.Select.Map {
				used = used || m.Schema == name
			}
		}
		if used {
			out = append(out, fmt.Sprintf("check %d (%s)", i+1, ch.Label()))
		}
	}
	return out
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
	allKnown := func(names []string, what string) error {
		for i, name := range names {
			if slices.Contains(names[:i], name) {
				return fmt.Errorf("%s names %s twice", what, name)
			}
			if err := known(name); err != nil {
				return err
			}
		}
		return nil
	}
	if ch.Schema != "" && len(ch.Schemas) > 0 {
		return errors.New("give one schema or a list of schemas, not both")
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
		if len(ch.Schemas) > 0 {
			return errors.New("with a value to pick by, the schemas come from its mapping and its default")
		}
		if err := allKnown(s.Default, "the default"); err != nil {
			return err
		}
	} else if ch.Schema == "" && len(ch.Schemas) == 0 {
		return errors.New("choose the schema")
	} else if err := allKnown(ch.alternatives(), "the check"); err != nil {
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
	names := ch.alternatives()
	if s := ch.Select; s != nil {
		value, found, err := s.value(r)
		if err != nil {
			// The body is not JSON: that is the verdict, whatever the schema.
			return auth.Verdict{Result: auth.Fail, Reason: err.Error(), Problems: []auth.Problem{{Keyword: "json", Message: err.Error()}}, Total: 1}
		}
		names = nil
		for _, m := range s.Map {
			if found && m.Value == value {
				names = []string{m.Schema}
				break
			}
		}
		switch {
		case names != nil:
		case len(s.Default) > 0:
			names = s.Default
		case !found:
			return auth.Verdict{Result: auth.NotChecked, Reason: "no " + s.Label() + " in the request"}
		default:
			return auth.Verdict{Result: auth.NotChecked, Reason: fmt.Sprintf("no schema for %s %q", s.Label(), value)}
		}
	}
	for _, name := range names {
		if !set.Has(name) {
			return auth.Verdict{Result: auth.NotVerifiable, Reason: "schema " + name + " does not exist"}
		}
	}
	results := make([]schema.Result, len(names))
	for i, name := range names {
		res := set.Validate(name, r.Body)
		if res.OK {
			v := auth.Verdict{Result: auth.Pass, Reason: "matches " + name, Images: res.Images}
			if n := schema.Undecoded(res.Images); n > 0 {
				// HEIC or AVIF: recognised, but whether it is whole cannot be told here.
				v.Result, v.Reason = auth.NotVerifiable, fmt.Sprintf("matches %s, but %d image(s) cannot be decoded here to check them", name, n)
			}
			return v
		}
		results[i] = res
	}
	// None matched: the problems of the closest one, and how far the others
	// were. Closest: the fewest problems at the top of the body (another
	// shape: wrong type, missing fields, which the validator reports as one
	// problem however many are missing), then the fewest in all, then the
	// first in order.
	closer := func(a, b schema.Result) bool {
		if ra, rb := rootProblems(a), rootProblems(b); ra != rb {
			return ra < rb
		}
		return a.Total < b.Total
	}
	best := 0
	for i, res := range results {
		if closer(res, results[best]) {
			best = i
		}
	}
	res := results[best]
	v := auth.Verdict{Result: auth.Fail, Problems: res.Problems, Total: res.Total, Images: res.Images}
	switch {
	case res.Total == 1 && res.Problems[0].Keyword == "json":
		v.Reason = res.Problems[0].Message // not JSON: the same for every schema
		return v
	case res.Total == 1:
		v.Reason = "1 error against " + names[best]
	default:
		v.Reason = strconv.Itoa(res.Total) + " errors against " + names[best]
	}
	if len(names) > 1 {
		var others []string
		for i, o := range results {
			if i != best {
				others = append(others, fmt.Sprintf("%s: %s", names[i], errorCount(o.Total)))
			}
		}
		v.Reason = "matches none of " + strings.Join(names, ", ") + "; closest " + names[best] + " with " +
			errorCount(res.Total) + " (" + strings.Join(others, ", ") + ")"
	}
	return v
}

func rootProblems(r schema.Result) int {
	n := 0
	for _, p := range r.Problems {
		if p.Path == "" {
			n++
		}
	}
	return n
}

func errorCount(n int) string {
	if n == 1 {
		return "1 error"
	}
	return strconv.Itoa(n) + " errors"
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
	if msg := schema.BodyError(r.Body); msg != "" {
		return "", false, errors.New(msg)
	}
	dec := json.NewDecoder(bytes.NewReader(r.Body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return "", false, fmt.Errorf("not JSON: %v", err) // not reached: BodyError checked it
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
