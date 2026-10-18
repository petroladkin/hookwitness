// Package setup is the portable form of endpoints' setup: a JSON file with
// each endpoint's path and behaviour and the names and kinds of its
// secrets, never a value. One instance exports it, another imports it; a
// replace keeps the secrets it already has values for.
package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/catch"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

// Format is the version of the file this build writes and reads; a file
// of a newer one is refused.
const Format = 1

// MaxSize bounds a setup file: schemas are up to 1 MiB each.
const MaxSize = 16 << 20

// File is a setup file: one or more endpoints.
type File struct {
	Hookwitness int        `json:"hookwitness"` // the format marker and version
	Endpoints   []Endpoint `json:"endpoints"`
}

// Endpoint is one endpoint's setup: its behaviour without secret values.
// Requests, counters, the on/off state, ids and times are not part of it.
type Endpoint struct {
	Path    string           `json:"path"`
	Default emulate.Reply    `json:"default"`
	Rules   []emulate.Rule   `json:"rules"`
	Checks  []emulate.Check  `json:"checks"`
	Vars    []emulate.Var    `json:"vars"`
	Schemas []emulate.Schema `json:"schemas"`
	Secrets []Secret         `json:"secrets"`
}

// Secret names a secret the endpoint needs, and its kind: "key" (a signing
// key) or "token" (a token or password).
type Secret struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Source is an endpoint to export.
type Source struct {
	Path   string
	Config emulate.Config
}

// Export makes the setup file of endpoints.
func Export(eps ...Source) File {
	f := File{Hookwitness: Format, Endpoints: []Endpoint{}}
	for _, ep := range eps {
		c := ep.Config
		e := Endpoint{Path: ep.Path, Default: c.Default,
			Rules: nonNil(c.Rules), Checks: nonNil(c.Checks), Vars: nonNil(c.Vars), Schemas: nonNil(c.Schemas), Secrets: []Secret{}}
		for _, s := range c.Secrets {
			e.Secrets = append(e.Secrets, Secret{Name: s.Name, Kind: s.Kind})
		}
		f.Endpoints = append(f.Endpoints, e)
	}
	return f
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// JSON is the file's bytes: indented, ending with a newline.
func (f File) JSON() []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(f)
	return b.Bytes()
}

// Options are what a file is checked against.
type Options struct {
	Reserved string        // the path prefix endpoints may not use (catch.CheckPath)
	MaxWait  time.Duration // the longest delay a reply may ask for
}

// Parse reads and checks a setup file: the format marker and version,
// no unknown fields, each path valid and once, each behaviour valid.
func Parse(data []byte, o Options) (File, error) {
	var head struct {
		Hookwitness json.RawMessage `json:"hookwitness"`
	}
	var version int
	if json.NewDecoder(bytes.NewReader(data)).Decode(&head) != nil || head.Hookwitness == nil || json.Unmarshal(head.Hookwitness, &version) != nil {
		return File{}, errors.New(`not a hookwitness setup file: it needs a JSON object with "hookwitness": 1 (export one to see the format)`)
	}
	switch {
	case version > Format:
		return File{}, fmt.Errorf("made by a newer hookwitness (format %d); this one reads format %d: update hookwitness", version, Format)
	case version < 1:
		return File{}, fmt.Errorf("format %d is not a hookwitness setup format", version)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f File
	if err := dec.Decode(&f); err != nil {
		return File{}, fmt.Errorf("the setup file: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return File{}, errors.New("the setup file has more data after the setup")
	}
	if len(f.Endpoints) == 0 {
		return File{}, errors.New("the setup file has no endpoints")
	}
	seen := map[string]bool{}
	for _, e := range f.Endpoints {
		if msg := catch.CheckPath(o.Reserved, e.Path); msg != "" {
			return File{}, fmt.Errorf("endpoint %q: %s", e.Path, msg)
		}
		if seen[e.Path] {
			return File{}, fmt.Errorf("%s is in the file twice", e.Path)
		}
		seen[e.Path] = true
		if err := e.Config(nil).Validate(o.MaxWait); err != nil {
			return File{}, fmt.Errorf("endpoint %s: %w", e.Path, err)
		}
	}
	return f, nil
}

// Config is the behaviour to store for the endpoint. Over an existing
// endpoint (old not nil) each secret of the same name and kind keeps its
// value; the file's other secrets are not set, and secrets the file does
// not name are dropped. Applying the same file twice gives the same result.
func (e Endpoint) Config(old *emulate.Config) emulate.Config {
	c := emulate.Config{Default: e.Default, Rules: e.Rules, Checks: e.Checks, Vars: e.Vars, Schemas: e.Schemas}
	if c.Default.Status == 0 {
		c.Default.Status = 200 // as stored configurations read
	}
	for _, s := range e.Secrets {
		ns := emulate.Secret{Name: s.Name, Kind: s.Kind}
		if old != nil {
			if have, ok := old.SecretByName(s.Name); ok && have.Kind == s.Kind {
				ns = have
			}
		}
		c.Secrets = append(c.Secrets, ns)
	}
	// Stored configurations omit empty lists: so does this one, so that an
	// import of an export stores the same text.
	if len(c.Rules) == 0 {
		c.Rules = nil
	}
	if len(c.Checks) == 0 {
		c.Checks = nil
	}
	if len(c.Vars) == 0 {
		c.Vars = nil
	}
	if len(c.Schemas) == 0 {
		c.Schemas = nil
	}
	return c
}

// Paths lists the file's paths in order.
func (f File) Paths() []string {
	var out []string
	for _, e := range f.Endpoints {
		out = append(out, e.Path)
	}
	return out
}

// Unset names the secrets of a stored behaviour that have no value.
func Unset(c emulate.Config) []string {
	out := []string{}
	for _, s := range c.Secrets {
		if !s.Set() {
			out = append(out, s.Name)
		}
	}
	return out
}

// FileName is the name to save one endpoint's setup under: the path's
// segments joined by "-", e.g. stripe-webhooks.hookwitness.json.
func FileName(path string) string {
	segs := slices.DeleteFunc(strings.Split(path, "/"), func(s string) bool { return s == "" })
	name := strings.Join(segs, "-")
	name = strings.Map(func(r rune) rune {
		if r == '~' {
			return '-'
		}
		return r
	}, name)
	if name == "" {
		name = "root"
	}
	return name + ".hookwitness.json"
}

// Importer stores imported endpoints (internal/store.Store).
type Importer interface {
	ImportEndpoints(ctx context.Context, items []store.ImportItem) ([]store.ImportResult, error)
}

// What an import did to an endpoint.
const (
	Created   = "created"
	Replaced  = "replaced"
	Unchanged = "unchanged" // replaced by the same setup
)

// Result is one endpoint of an import.
type Result struct {
	ID     int64    `json:"id"`
	Path   string   `json:"path"`
	Result string   `json:"result"` // Created, Replaced or Unchanged
	Unset  []string `json:"secrets_not_set"`
}

// Apply imports a parsed file, all of it or nothing. A path that is
// already an endpoint fails with store.ErrDuplicatePath unless replace.
func Apply(ctx context.Context, st Importer, f File, replace bool, o Options) ([]Result, error) {
	items := make([]store.ImportItem, 0, len(f.Endpoints))
	stored := make([]emulate.Config, len(f.Endpoints))
	for i, e := range f.Endpoints {
		items = append(items, store.ImportItem{Path: e.Path, Replace: replace, Config: func(old string, exists bool) (string, error) {
			var have *emulate.Config
			if exists {
				c, err := emulate.Parse(old)
				if err != nil {
					return "", fmt.Errorf("endpoint %s: %w", e.Path, err)
				}
				have = &c
			}
			c := e.Config(have)
			if err := c.Validate(o.MaxWait); err != nil {
				return "", &InvalidError{fmt.Errorf("endpoint %s with the secrets it has: %w", e.Path, err)}
			}
			stored[i] = c
			return c.String(), nil
		}})
	}
	rs, err := st.ImportEndpoints(ctx, items)
	if err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(rs))
	for i, r := range rs {
		res := Result{ID: r.ID, Path: r.Path, Result: Replaced, Unset: Unset(stored[i])}
		switch {
		case r.Created:
			res.Result = Created
		case !r.Changed:
			res.Result = Unchanged
		}
		out = append(out, res)
	}
	return out, nil
}

// InvalidError is a setup that cannot be applied as it is (not a failure
// of the server).
type InvalidError struct{ Err error }

func (e *InvalidError) Error() string { return e.Err.Error() }
func (e *InvalidError) Unwrap() error { return e.Err }
