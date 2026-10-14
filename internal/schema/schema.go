// Package schema validates request bodies against an endpoint's JSON
// Schemas. It is pure and offline: schemas are compiled together, refer to
// each other by name, and never load anything from the network or the
// disk. Drafts follow $schema (2020-12 when absent); the format keyword is
// asserted.
package schema

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
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
	assertImages(c)
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
			ps, total := sortProblems(problems(ve))
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
	Images   []Image   // the images the schema declares, found in the body
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
	sch := s.byName[name]
	res := Result{OK: true}
	var found []ranked
	if err := sch.Validate(inst); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			found = problems(ve)
		} else {
			found = []ranked{{Problem: Problem{Message: err.Error()}}}
		}
	}
	res.Images = findImages(sch, inst)
	for _, p := range dataURLProblems(res.Images) {
		found = append(found, ranked{p, tokens(p.Path), p.Keyword})
	}
	if len(found) > 0 {
		res.OK = false
		res.Problems, res.Total = sortProblems(found)
	}
	return res
}

// BodyError says why body is not one JSON document, as a schema verdict
// does ("empty body", "not JSON: ... at byte N"); "" when it is.
func BodyError(body []byte) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return "empty body"
	}
	if _, err := jsonschema.UnmarshalJSON(bytes.NewReader(body)); err != nil {
		return "not JSON: " + jsonError(string(body), err)
	}
	return ""
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

// problems lists the leaf errors of a validation, every one: the caller
// sorts them (sortProblems) before keeping MaxProblems.
func problems(ve *jsonschema.ValidationError) []ranked {
	var out []ranked
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			kw := e.ErrorKind.KeywordPath()
			p := Problem{Path: pointer(e.InstanceLocation), Message: e.ErrorKind.LocalizedString(printer)}
			if k, ok := e.ErrorKind.(*kind.ContentMediaType); ok && k.Err != nil {
				p.Message = "not a valid " + k.Want + ": " + k.Err.Error()
			}
			if k, ok := e.ErrorKind.(*kind.ContentEncoding); ok && k.Err != nil {
				p.Message = k.Err.Error()
			}
			if len(kw) > 0 {
				p.Keyword = kw[len(kw)-1]
			}
			out = append(out, ranked{p, e.InstanceLocation, strings.Join(kw, "/")})
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	return out
}

// ranked is a problem with what it is sorted by.
type ranked struct {
	Problem
	loc     []string // the instance location's tokens
	keyword string   // the keyword path
}

// sortProblems puts problems in one order whatever order the validator
// found them in (it walks a schema's properties in map order): by place in
// the body, array indexes as numbers, then by keyword path and message.
// It returns the first MaxProblems and the count of all.
func sortProblems(rs []ranked) ([]Problem, int) {
	slices.SortStableFunc(rs, func(a, b ranked) int {
		for i := 0; i < len(a.loc) && i < len(b.loc); i++ {
			if c := compareToken(a.loc[i], b.loc[i]); c != 0 {
				return c
			}
		}
		if c := cmp.Compare(len(a.loc), len(b.loc)); c != 0 {
			return c
		}
		if c := strings.Compare(a.keyword, b.keyword); c != 0 {
			return c
		}
		return strings.Compare(a.Message, b.Message)
	})
	out := make([]Problem, 0, min(len(rs), MaxProblems))
	for _, r := range rs[:min(len(rs), MaxProblems)] {
		out = append(out, r.Problem)
	}
	return out, len(rs)
}

// compareToken orders two location tokens: numbers (array indexes) by
// value and before names, names as strings.
func compareToken(a, b string) int {
	na, ea := strconv.Atoi(a)
	nb, eb := strconv.Atoi(b)
	switch {
	case ea == nil && eb == nil:
		return cmp.Compare(na, nb)
	case ea == nil:
		return -1
	case eb == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// tokens splits a JSON pointer into its tokens.
func tokens(ptr string) []string {
	if ptr == "" {
		return nil
	}
	out := strings.Split(ptr[1:], "/")
	for i, t := range out {
		out[i] = strings.NewReplacer("~1", "/", "~0", "~").Replace(t)
	}
	return out
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
