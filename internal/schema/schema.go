// Package schema validates request bodies against an endpoint's JSON
// Schemas. It is pure and offline: schemas are compiled together, refer to
// each other by name, and never load anything from the network or the
// disk. Drafts follow $schema (2020-12 when absent); the format keyword is
// asserted.
package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/petroladkin/hookwitness/internal/auth"
)

// Base is the base URI of an endpoint's schemas: the schema named Order is
// the resource Base+"Order", so {"$ref": "Order"} in another one finds it.
const Base = "https://schemas.hookwitness.invalid/"

// MaxProblems is how many problems a verdict lists; the total counts all.
const MaxProblems = 20

// Doc is one stored schema.
type Doc struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

// Problem is one reason a body does not match, as verdicts record it.
type Problem = auth.Problem

// Set is an endpoint's schemas, compiled.
type Set struct {
	byName map[string]*jsonschema.Schema
}

var nameRule = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

var printer = message.NewPrinter(language.English)

// refuse is the URL loader: nothing outside the endpoint's own schemas.
type refuse struct{}

func (refuse) Load(url string) (any, error) {
	return nil, errors.New("not one of this endpoint's schemas")
}

// Compile compiles docs together. An error names the schema at fault and
// says why, for the person who pasted it.
func Compile(docs []Doc) (*Set, error) {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	c.UseLoader(refuse{})
	seen := map[string]bool{}
	for _, d := range docs {
		if !nameRule.MatchString(d.Name) {
			return nil, fmt.Errorf("schema %q: a name is letters, digits and _ . -, not starting with a digit", d.Name)
		}
		if seen[d.Name] {
			return nil, fmt.Errorf("schema %s is defined twice", d.Name)
		}
		seen[d.Name] = true
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(d.Text))
		if err != nil {
			return nil, fmt.Errorf("schema %s is not valid JSON: %s", d.Name, jsonError(d.Text, err))
		}
		if err := c.AddResource(Base+d.Name, doc); err != nil {
			return nil, fmt.Errorf("schema %s: %v", d.Name, err)
		}
	}
	set := &Set{byName: map[string]*jsonschema.Schema{}}
	for _, d := range docs {
		sch, err := c.Compile(Base + d.Name)
		if err != nil {
			return nil, fmt.Errorf("schema %s: %s", d.Name, compileError(err))
		}
		set.byName[d.Name] = sch
	}
	return set, nil
}

// compileError says in a line or a few why a schema does not compile.
func compileError(err error) string {
	var load *jsonschema.LoadURLError
	var meta *jsonschema.SchemaValidationError
	switch {
	case errors.As(err, &load):
		ref := strings.TrimPrefix(load.URL, Base)
		if ref != load.URL {
			return fmt.Sprintf("$ref to %s: there is no schema named %s on this endpoint", ref, ref)
		}
		return fmt.Sprintf("$ref to %s: schemas are never fetched; refer to this endpoint's schemas by name, e.g. {\"$ref\": \"Address\"}", load.URL)
	case errors.As(err, &meta):
		var ve *jsonschema.ValidationError
		if errors.As(meta.Err, &ve) {
			ps, total := problems(ve)
			var parts []string
			for _, p := range ps {
				where := p.Path
				if where == "" {
					where = "(root)"
				}
				parts = append(parts, where+": "+p.Message)
			}
			s := "not a valid schema: " + strings.Join(parts, "; ")
			if total > len(ps) {
				s += fmt.Sprintf(" (%d problems in all)", total)
			}
			return s
		}
		return "not a valid schema: " + meta.Err.Error()
	}
	return strings.ReplaceAll(err.Error(), Base, "")
}

// Has reports whether a schema of that name exists.
func (s *Set) Has(name string) bool { _, ok := s.byName[name]; return ok }

// Result is a body checked against one schema.
type Result struct {
	OK       bool
	Problems []Problem // at most MaxProblems
	Total    int       // all problems found
}

// Validate checks body against the schema called name, which must exist.
func (s *Set) Validate(name string, body []byte) Result {
	if len(bytes.TrimSpace(body)) == 0 {
		return Result{Problems: []Problem{{Keyword: "json", Message: "empty body"}}, Total: 1}
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return Result{Problems: []Problem{{Keyword: "json", Message: "not JSON: " + jsonError(string(body), err)}}, Total: 1}
	}
	err = s.byName[name].Validate(inst)
	if err == nil {
		return Result{OK: true}
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return Result{Problems: []Problem{{Message: err.Error()}}, Total: 1}
	}
	ps, total := problems(ve)
	return Result{Problems: ps, Total: total}
}

// jsonError describes a JSON syntax error with the offset of the byte it
// stopped at.
func jsonError(text string, err error) string {
	var v any
	jerr := json.Unmarshal([]byte(text), &v)
	var syn *json.SyntaxError
	if errors.As(jerr, &syn) {
		// Offset counts the bytes read, the offending one included; name
		// that byte, counted from 0.
		return fmt.Sprintf("%s at byte %d", syn.Error(), max(syn.Offset-1, 0))
	}
	if jerr != nil {
		return jerr.Error()
	}
	return err.Error() // valid for encoding/json, refused by the library (trailing data)
}

// problems flattens a validation error to its leaves, in document order.
func problems(ve *jsonschema.ValidationError) ([]Problem, int) {
	var out []Problem
	total := 0
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			total++
			if len(out) < MaxProblems {
				kw := e.ErrorKind.KeywordPath()
				p := Problem{Path: pointer(e.InstanceLocation), Message: e.ErrorKind.LocalizedString(printer)}
				if len(kw) > 0 {
					p.Keyword = kw[len(kw)-1]
				}
				out = append(out, p)
			}
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	return out, total
}

// pointer is a JSON pointer (RFC 6901) from path tokens.
func pointer(tokens []string) string {
	var sb strings.Builder
	for _, t := range tokens {
		sb.WriteByte('/')
		sb.WriteString(strings.NewReplacer("~", "~0", "/", "~1").Replace(t))
	}
	return sb.String()
}

// The compiled sets of recent configurations: compiling on every request
// would cost more than validating.
var (
	cacheMu sync.Mutex
	cache   = map[[32]byte]cached{}
)

type cached struct {
	set *Set
	err error
}

// Cached is Compile, remembered for the same schemas.
func Cached(docs []Doc) (*Set, error) {
	h := sha256.New()
	for _, d := range docs {
		fmt.Fprintf(h, "%d:%s%d:%s", len(d.Name), d.Name, len(d.Text), d.Text)
	}
	var key [32]byte
	copy(key[:], h.Sum(nil))
	cacheMu.Lock()
	c, ok := cache[key]
	cacheMu.Unlock()
	if ok {
		return c.set, c.err
	}
	set, err := Compile(docs)
	cacheMu.Lock()
	if len(cache) >= 64 {
		clear(cache) // a few endpoints are edited at a time; start again
	}
	cache[key] = cached{set, err}
	cacheMu.Unlock()
	return set, err
}
