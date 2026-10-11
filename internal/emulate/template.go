package emulate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"text/template"
)

// maxRendered bounds a rendered body, so a template cannot make the server
// build an unbounded reply.
const maxRendered = 1 << 20

func parseTemplate(s string) (*template.Template, error) {
	t, err := template.New("").Option("missingkey=zero").Parse(s)
	if err != nil {
		// "template: :1: unexpected ..." -> "line 1: unexpected ..."
		msg := strings.TrimPrefix(err.Error(), "template: :")
		return nil, errors.New("template error at line " + msg)
	}
	return t, nil
}

// Data is what a template sees, e.g.
//
//	{{.Header "X-Request-Id"}}  {{.Query "challenge"}}  {{.JSON "event.id"}}
//	{{.Var.TOKEN}}  {{.N}}  {{.Method}}  {{.Path}}
type Data struct {
	Method string
	Path   string
	Body   string
	Var    map[string]string
	N      int64 // the request's number since the counter was reset; 0 when refused
	req    Request
	query  url.Values
	json   any
	parsed bool
}

// NewData prepares the template data of a request.
func NewData(r Request, vars []Var, n int64) *Data {
	d := &Data{Method: r.Method, Path: r.Path, Body: string(r.Body), Var: map[string]string{}, N: n, req: r}
	for _, v := range vars {
		d.Var[v.Name] = v.Value
	}
	d.query, _ = url.ParseQuery(r.RawQuery)
	return d
}

// Header is the first value of a request header, or "".
func (d *Data) Header(name string) string {
	if vs := headerValues(d.req.Headers, name); len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// Query is the first value of a query parameter, or "".
func (d *Data) Query(name string) string { return d.query.Get(name) }

// JSON is the value at a dotted path in the JSON body ("a.b.0.c"): a string
// as it is, anything else as compact JSON, "" when absent or not JSON.
func (d *Data) JSON(path string) string {
	if !d.parsed {
		d.parsed = true
		dec := json.NewDecoder(bytes.NewReader(d.req.Body))
		dec.UseNumber()
		if dec.Decode(&d.json) != nil {
			d.json = nil
		}
	}
	v := d.json
	if path != "" {
		for _, key := range strings.Split(path, ".") {
			switch node := v.(type) {
			case map[string]any:
				v = node[key]
			case []any:
				i, err := strconv.Atoi(key)
				if err != nil || i < 0 || i >= len(node) {
					return ""
				}
				v = node[i]
			default:
				return ""
			}
		}
	}
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// Rendered is a reply ready to send.
type Rendered struct {
	Status  int
	Headers []Header
	Body    []byte
}

// errTooLarge is returned when a template renders more than maxRendered.
var errTooLarge = fmt.Errorf("the rendered body is over %d bytes", maxRendered)

type capped struct{ bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if c.Len()+len(p) > maxRendered {
		return 0, errTooLarge
	}
	return c.Buffer.Write(p)
}

// Render fills the reply's templates with d.
func (r Reply) Render(d *Data) (Rendered, error) {
	out := Rendered{Status: r.Status}
	for _, h := range r.Headers {
		v, err := render(h.Value, d)
		if err != nil {
			return Rendered{}, fmt.Errorf("header %s: %w", h.Name, err)
		}
		if strings.ContainsAny(v, "\r\n") {
			return Rendered{}, fmt.Errorf("header %s: the rendered value has a line break", h.Name)
		}
		out.Headers = append(out.Headers, Header{h.Name, v})
	}
	body, err := render(r.Body, d)
	if err != nil {
		return Rendered{}, fmt.Errorf("body: %w", err)
	}
	out.Body = []byte(body)
	return out, nil
}

func render(src string, d *Data) (string, error) {
	if !strings.Contains(src, "{{") {
		return src, nil
	}
	t, err := parseTemplate(src)
	if err != nil {
		return "", err
	}
	var buf capped
	if err := t.Execute(&buf, d); err != nil {
		return "", errors.New(strings.TrimPrefix(err.Error(), "template: "))
	}
	return buf.String(), nil
}

// ParseHeaders reads response headers written one per line as "Name: value"
// (the way they look on the wire); blank lines are skipped.
func ParseHeaders(text string) ([]Header, error) {
	var out []Header
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("header line %d: write it as Name: value", i+1)
		}
		out = append(out, Header{strings.TrimSpace(name), strings.TrimSpace(value)})
	}
	return out, nil
}

// FormatHeaders is the inverse of ParseHeaders.
func FormatHeaders(hs []Header) string {
	var sb strings.Builder
	for _, h := range hs {
		sb.WriteString(h.Name + ": " + h.Value + "\n")
	}
	return sb.String()
}

// ParseVars reads variables written one per line as NAME=value.
func ParseVars(text string) ([]Var, error) {
	var out []Var
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("variable line %d: write it as NAME=value", i+1)
		}
		out = append(out, Var{strings.TrimSpace(name), strings.TrimSpace(value)})
	}
	return out, nil
}

// FormatVars is the inverse of ParseVars.
func FormatVars(vs []Var) string {
	var sb strings.Builder
	for _, v := range vs {
		sb.WriteString(v.Name + "=" + v.Value + "\n")
	}
	return sb.String()
}
