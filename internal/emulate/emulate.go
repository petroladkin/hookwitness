// Package emulate decides how an endpoint answers a caught request: request
// checks first, then the first matching rule, else the default reply. It is
// pure: it holds the configuration, validates it, picks the outcome and
// renders the reply; the catcher carries the outcome out on the wire.
package emulate

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Header is one response header line.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"` // a template
}

// Reply is a response: status, headers in order, a body, and how long to
// wait before sending it.
type Reply struct {
	Status   int      `json:"status"`
	Headers  []Header `json:"headers,omitempty"`
	Body     string   `json:"body,omitempty"` // a template
	DelayMs  int64    `json:"delay_ms,omitempty"`
	JitterMs int64    `json:"jitter_ms,omitempty"` // random extra delay, 0..JitterMs
}

// Action kinds.
const (
	ActReply   = "reply"
	ActTimeout = "timeout" // never answer; hold until the sender leaves or the cap
	ActDrop    = "drop"    // reset the connection without a response
)

// Condition kinds.
const (
	WhenEvery  = "every"  // every Nth request: N, 2N, 3N...
	WhenFirst  = "first"  // the first N requests since the counter was reset
	WhenRandom = "random" // with a probability of Percent
)

// Condition says which requests a rule takes.
type Condition struct {
	Kind    string  `json:"kind"`
	N       int64   `json:"n,omitempty"`
	Percent float64 `json:"percent,omitempty"`
}

// Rule is a condition and what to do when it holds.
type Rule struct {
	When  Condition `json:"when"`
	Do    string    `json:"do"`              // ActReply, ActTimeout or ActDrop
	Reply Reply     `json:"reply,omitempty"` // for ActReply
}

// Check match kinds.
const (
	MatchPresent = "present"
	MatchEquals  = "equals"
	MatchRegex   = "regex"
)

// Check is a request check: here a required header. A request that fails a
// check is refused with Error, or the standard 400 when Error is nil.
type Check struct {
	Header string `json:"header"`
	Match  string `json:"match"`
	Value  string `json:"value,omitempty"` // for MatchEquals and MatchRegex
	Error  *Reply `json:"error,omitempty"`
}

// Var is an endpoint variable: a test value in plain text, never a secret.
type Var struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Config is an endpoint's behaviour.
type Config struct {
	Default Reply   `json:"default"`
	Rules   []Rule  `json:"rules,omitempty"`
	Checks  []Check `json:"checks,omitempty"`
	Vars    []Var   `json:"vars,omitempty"`
}

// DefaultConfig is what a new endpoint does: 200 with an empty body.
func DefaultConfig() Config { return Config{Default: Reply{Status: 200}} }

// Parse reads a stored configuration; empty means DefaultConfig.
func Parse(s string) (Config, error) {
	if strings.TrimSpace(s) == "" || s == "{}" {
		return DefaultConfig(), nil
	}
	var c Config
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return Config{}, fmt.Errorf("endpoint behaviour: %w", err)
	}
	if c.Default.Status == 0 {
		c.Default.Status = 200
	}
	return c, nil
}

// String is the stored form.
func (c Config) String() string {
	b, _ := json.Marshal(c)
	return string(b)
}

// framing are the response headers the server owns (Q6).
var framing = map[string]bool{"content-length": true, "transfer-encoding": true, "connection": true}

var (
	headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	varName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Validate checks the whole configuration against maxWait, the longest
// delay or hold allowed. Errors name the part at fault, for the person
// editing it.
func (c Config) Validate(maxWait time.Duration) error {
	seen := map[string]bool{}
	for _, v := range c.Vars {
		if !varName.MatchString(v.Name) {
			return fmt.Errorf("variable %q: a name is letters, digits and _, not starting with a digit", v.Name)
		}
		if seen[v.Name] {
			return fmt.Errorf("variable %q is defined twice", v.Name)
		}
		seen[v.Name] = true
	}
	if err := c.Default.validate(maxWait); err != nil {
		return fmt.Errorf("default reply: %w", err)
	}
	for i, r := range c.Rules {
		if err := r.validate(maxWait); err != nil {
			return fmt.Errorf("rule %d: %w", i+1, err)
		}
	}
	for i, ch := range c.Checks {
		if err := ch.validate(maxWait); err != nil {
			return fmt.Errorf("check %d: %w", i+1, err)
		}
	}
	return nil
}

func (r Reply) validate(maxWait time.Duration) error {
	if r.Status < 200 || r.Status > 599 {
		return fmt.Errorf("status %d: use 200-599", r.Status)
	}
	for _, h := range r.Headers {
		if !headerName.MatchString(h.Name) {
			return fmt.Errorf("header name %q is not a valid HTTP header name", h.Name)
		}
		if framing[strings.ToLower(h.Name)] {
			return fmt.Errorf("header %s is set by the server (message framing) and cannot be set here", h.Name)
		}
		if badHeaderValue(h.Value) {
			return fmt.Errorf("header %s: a value is one line without control characters", h.Name)
		}
		if _, err := parseTemplate(h.Value); err != nil {
			return fmt.Errorf("header %s: %w", h.Name, err)
		}
	}
	if _, err := parseTemplate(r.Body); err != nil {
		return fmt.Errorf("body: %w", err)
	}
	if r.DelayMs < 0 || r.JitterMs < 0 {
		return errors.New("delay and jitter cannot be negative")
	}
	// Each part is checked alone first, so the sum cannot overflow.
	limit := maxWait.Milliseconds()
	if r.DelayMs > limit || r.JitterMs > limit || r.DelayMs+r.JitterMs > limit {
		return fmt.Errorf("delay plus jitter is over the maximum wait of %s", maxWait)
	}
	return nil
}

func (r Rule) validate(maxWait time.Duration) error {
	switch r.When.Kind {
	case WhenEvery, WhenFirst:
		if r.When.N < 1 {
			return errors.New("the count N must be at least 1")
		}
	case WhenRandom:
		if r.When.Percent <= 0 || r.When.Percent > 100 {
			return errors.New("the percentage must be above 0 and at most 100")
		}
	default:
		return fmt.Errorf("unknown condition %q", r.When.Kind)
	}
	switch r.Do {
	case ActReply:
		return r.Reply.validate(maxWait)
	case ActTimeout, ActDrop:
		return nil
	}
	return fmt.Errorf("unknown action %q", r.Do)
}

func (ch Check) validate(maxWait time.Duration) error {
	if !headerName.MatchString(ch.Header) {
		return fmt.Errorf("header name %q is not a valid HTTP header name", ch.Header)
	}
	switch ch.Match {
	case MatchPresent:
	case MatchEquals:
	case MatchRegex:
		if _, err := regexp.Compile(ch.Value); err != nil {
			return fmt.Errorf("regular expression: %w", err)
		}
	default:
		return fmt.Errorf("unknown match %q", ch.Match)
	}
	if ch.Error != nil {
		if err := ch.Error.validate(maxWait); err != nil {
			return fmt.Errorf("error reply: %w", err)
		}
	}
	return nil
}

// badHeaderValue reports control characters other than a tab, which a
// header value may not carry (RFC 9110, 5.5).
func badHeaderValue(v string) bool {
	return strings.IndexFunc(v, func(r rune) bool { return (r < 0x20 && r != '\t') || r == 0x7f }) >= 0
}

// Label is a short description of the condition: "every 3rd", "first 2",
// "10%".
func (c Condition) Label() string {
	switch c.Kind {
	case WhenEvery:
		if c.N == 1 {
			return "every request"
		}
		return "every " + ordinal(c.N)
	case WhenFirst:
		return "first " + strconv.FormatInt(c.N, 10)
	case WhenRandom:
		return strconv.FormatFloat(c.Percent, 'f', -1, 64) + "%"
	}
	return c.Kind
}

func ordinal(n int64) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.FormatInt(n, 10) + suffix
}

// holds reports whether the condition takes the request numbered n (1-based,
// counted since the last reset).
func (c Condition) holds(n int64) bool {
	switch c.Kind {
	case WhenEvery:
		return c.N > 0 && n%c.N == 0
	case WhenFirst:
		return n <= c.N
	case WhenRandom:
		return rand.Float64()*100 < c.Percent
	}
	return false
}

// Request is what checks and templates see of a caught request.
type Request struct {
	Method   string
	Path     string
	RawQuery string
	Headers  []Header // as received
	Body     []byte
}

// Failed is a request check that did not pass.
type Failed struct {
	Index int // 1-based
	Check Check
	Why   string // "missing" or "wrong value"
}

// Describe names the failed check for the record and the standard error.
func (f Failed) Describe() string {
	return fmt.Sprintf("check %d: header %s %s", f.Index, f.Check.Header, f.Why)
}

// RunChecks returns the first failed check, or nil.
func (c Config) RunChecks(r Request) *Failed {
	for i, ch := range c.Checks {
		values := headerValues(r.Headers, ch.Header)
		why := ""
		switch {
		case len(values) == 0:
			why = "missing"
		case ch.Match == MatchEquals && !contains(values, ch.Value):
			why = "has a wrong value"
		case ch.Match == MatchRegex && !anyMatch(values, ch.Value):
			why = "has a wrong value"
		}
		if why != "" {
			return &Failed{Index: i + 1, Check: ch, Why: why}
		}
	}
	return nil
}

func headerValues(hs []Header, name string) []string {
	var out []string
	for _, h := range hs {
		if strings.EqualFold(h.Name, name) {
			out = append(out, h.Value)
		}
	}
	return out
}

func contains(vs []string, v string) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}

func anyMatch(vs []string, expr string) bool {
	re, err := regexp.Compile(expr)
	if err != nil {
		return false
	}
	for _, v := range vs {
		if re.MatchString(v) {
			return true
		}
	}
	return false
}

// Choice is what the rules decided for a request that passed the checks.
type Choice struct {
	Rule  int    // 1-based; 0 for the default reply
	Label string // the rule's condition, empty for the default
	Do    string
	Reply Reply // for ActReply
}

// Choose picks the first rule whose condition holds for the request
// numbered n, else the default reply.
func (c Config) Choose(n int64) Choice {
	for i, r := range c.Rules {
		if r.When.holds(n) {
			return Choice{Rule: i + 1, Label: r.When.Label(), Do: r.Do, Reply: r.Reply}
		}
	}
	return Choice{Do: ActReply, Reply: c.Default}
}

// Refusal is the reply to a request that failed a check.
func (f Failed) Refusal() Reply {
	if f.Check.Error != nil {
		return *f.Check.Error
	}
	return Reply{
		Status:  400,
		Headers: []Header{{Name: "Content-Type", Value: "text/plain; charset=utf-8"}},
		Body:    "Request refused: header " + f.Check.Header + " " + f.Why + ".\n",
	}
}

// Delay is how long to wait before replying: the fixed delay plus a random
// part of the jitter, never more than maxWait (a configuration saved under a
// higher limit still obeys the current one).
func (r Reply) Delay(maxWait time.Duration) time.Duration {
	limit := maxWait.Milliseconds()
	d := min(max(r.DelayMs, 0), limit)
	if j := min(max(r.JitterMs, 0), limit); j > 0 {
		d += rand.Int64N(j + 1)
	}
	return time.Duration(min(d, limit)) * time.Millisecond
}
