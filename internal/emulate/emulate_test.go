package emulate

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

const wait = 5 * time.Minute

func TestValidate(t *testing.T) {
	ok := Config{
		Default: Reply{Status: 202, Headers: []Header{{"X-Trace", "{{.Header \"X-Request-Id\"}}"}, {"X-Trace", "2"}}, Body: "{{.JSON \"challenge\"}}"},
		Rules: []Rule{
			{When: Condition{Kind: WhenEvery, N: 3}, Do: ActReply, Reply: Reply{Status: 500, DelayMs: 100, JitterMs: 50}},
			{When: Condition{Kind: WhenFirst, N: 2}, Do: ActTimeout},
			{When: Condition{Kind: WhenRandom, Percent: 12.5}, Do: ActDrop},
		},
		Checks: []Check{{Header: "X-Token", Match: MatchRegex, Value: "^tok_", Error: &Reply{Status: 401, Body: "no"}}},
		Vars:   []Var{{"TOKEN", "tok_test"}, {"_x1", ""}},
	}
	if err := ok.Validate(wait); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	bad := map[string]func(c *Config){
		"status 199":                      func(c *Config) { c.Default.Status = 199 },
		"status 600":                      func(c *Config) { c.Default.Status = 600 },
		"set by the server":               func(c *Config) { c.Default.Headers = []Header{{"content-length", "1"}} },
		"valid HTTP header":               func(c *Config) { c.Default.Headers = []Header{{"Bad Name", "1"}} },
		"template error":                  func(c *Config) { c.Default.Body = "{{.Header" },
		"body or body_base64, not both":   func(c *Config) { c.Default.BodyBase64 = "//4=" },
		"body_base64 is not valid base64": func(c *Config) { c.Default.Body, c.Default.BodyBase64 = "", "not base64!" },
		"body_base64 is over": func(c *Config) {
			c.Default.Body, c.Default.BodyBase64 = "", base64.StdEncoding.EncodeToString(make([]byte, maxRendered+1))
		},
		"maximum wait":                   func(c *Config) { c.Rules[0].Reply.DelayMs = int64(wait / time.Millisecond) },
		"cannot be negative":             func(c *Config) { c.Default.DelayMs = -1 },
		"the count N must be at least 1": func(c *Config) { c.Rules[0].When.N = 0 },
		"percentage":                     func(c *Config) { c.Rules[2].When.Percent = 101 },
		"unknown condition":              func(c *Config) { c.Rules[0].When.Kind = "sometimes" },
		"unknown action":                 func(c *Config) { c.Rules[1].Do = "explode" },
		"regular expression":             func(c *Config) { c.Checks[0].Value = "(" },
		"defined twice":                  func(c *Config) { c.Vars = append(c.Vars, Var{"TOKEN", "x"}) },
		"letters, digits and _":          func(c *Config) { c.Vars[0].Name = "1x" },
		"error reply":                    func(c *Config) { c.Checks[0].Error.Status = 100 },
		"rule 2":                         func(c *Config) { c.Rules[1].When.N = -1 },
	}
	for want, mutate := range bad {
		c := ok
		c.Default.Headers = append([]Header(nil), ok.Default.Headers...)
		c.Rules = append([]Rule(nil), ok.Rules...)
		c.Vars = append([]Var(nil), ok.Vars...)
		errReply := *ok.Checks[0].Error
		c.Checks = []Check{ok.Checks[0]}
		c.Checks[0].Error = &errReply
		mutate(&c)
		err := c.Validate(wait)
		if want == "nested range" {
			want = "range is not supported"
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}

func TestChoose(t *testing.T) {
	c := Config{
		Default: Reply{Status: 200},
		Rules: []Rule{
			{When: Condition{Kind: WhenFirst, N: 2}, Do: ActReply, Reply: Reply{Status: 503}},
			{When: Condition{Kind: WhenEvery, N: 3}, Do: ActDrop},
		},
	}
	var got []string
	for n := int64(1); n <= 7; n++ {
		ch := c.Choose(n)
		got = append(got, fmt.Sprintf("%d:%s/%d", ch.Rule, ch.Do, ch.Reply.Status))
	}
	want := "[1:reply/503 1:reply/503 2:drop/0 0:reply/200 0:reply/200 2:drop/0 0:reply/200]"
	if fmt.Sprint(got) != want {
		t.Fatalf("got %v\nwant %s (first match wins, default otherwise)", got, want)
	}
	random := Config{Default: Reply{Status: 200}, Rules: []Rule{{When: Condition{Kind: WhenRandom, Percent: 100}, Do: ActTimeout}}}
	if random.Choose(1).Do != ActTimeout {
		t.Fatal("100% must always hold")
	}
	hits := 0
	random.Rules[0].When.Percent = 30
	for n := range int64(10000) {
		if random.Choose(n).Rule == 1 {
			hits++
		}
	}
	if hits < 2500 || hits > 3500 {
		t.Fatalf("30%% rule held %d times in 10000", hits)
	}
}

func TestLabels(t *testing.T) {
	for c, want := range map[Condition]string{
		{Kind: WhenEvery, N: 1}:          "every request",
		{Kind: WhenEvery, N: 2}:          "every 2nd",
		{Kind: WhenEvery, N: 3}:          "every 3rd",
		{Kind: WhenEvery, N: 11}:         "every 11th",
		{Kind: WhenEvery, N: 21}:         "every 21st",
		{Kind: WhenFirst, N: 2}:          "first 2",
		{Kind: WhenRandom, Percent: 2.5}: "2.5%",
	} {
		if got := c.Label(); got != want {
			t.Errorf("%+v: %q, want %q", c, got, want)
		}
	}
}

func TestChecks(t *testing.T) {
	c := Config{Checks: []Check{
		{Header: "X-Token", Match: MatchPresent},
		{Header: "X-Env", Match: MatchEquals, Value: "test"},
		{Header: "X-Sig", Match: MatchRegex, Value: `^v1=[0-9a-f]+$`},
	}}
	req := func(hs ...string) Request {
		var h []Header
		for i := 0; i+1 < len(hs); i += 2 {
			h = append(h, Header{hs[i], hs[i+1]})
		}
		return Request{Headers: h}
	}
	if f := c.RunChecks(req("x-token", "1", "X-ENV", "test", "x-sig", "v1=ab12")); f != nil {
		t.Fatalf("passing request failed: %s", f.Describe())
	}
	cases := map[string]Request{
		"check 1: header X-Token missing":         req("X-Env", "test", "X-Sig", "v1=00"),
		"check 2: header X-Env has a wrong value": req("X-Token", "", "X-Env", "prod", "X-Sig", "v1=00"),
		"check 3: header X-Sig has a wrong value": req("X-Token", "1", "X-Env", "test", "X-Sig", "v2=00"),
	}
	for want, r := range cases {
		f := c.RunChecks(r)
		if f == nil || f.Describe() != want {
			t.Errorf("want %q, got %+v", want, f)
		}
	}
	// A repeated header passes if any value matches.
	if f := c.RunChecks(req("X-Token", "1", "X-Env", "prod", "X-Env", "test", "X-Sig", "v1=00")); f != nil {
		t.Errorf("repeated header: %s", f.Describe())
	}
	std := (&Failed{Index: 1, Check: c.Checks[0], Why: "missing"}).Refusal()
	if std.Status != 400 || !strings.Contains(std.Body, "X-Token missing") {
		t.Errorf("standard refusal %+v", std)
	}
	own := Check{Header: "A", Match: MatchPresent, Error: &Reply{Status: 401, Body: "who are you"}}
	if r := (&Failed{Check: own}).Refusal(); r.Status != 401 || r.Body != "who are you" {
		t.Errorf("own refusal %+v", r)
	}
}

func TestRender(t *testing.T) {
	r := Request{
		Method: "POST", Path: "/slack/events", RawQuery: "x=1&challenge=q",
		Headers: []Header{{"x-request-id", "abc"}, {"X-Request-Id", "second"}},
		Body:    []byte(`{"challenge":"3eZbrw1aB","event":{"items":[{"id":7},{"id":"eight"}],"n":1.50}}`),
	}
	d := NewData(r, []Var{{"TOKEN", "tok_test"}}, 42)
	reply := Reply{Status: 200, Headers: []Header{{"X-Echo", `{{.Header "X-Request-ID"}}`}, {"X-N", "{{.N}}"}},
		Body: `{{.JSON "challenge"}}|{{.JSON "event.items.1.id"}}|{{.JSON "event.items.0"}}|{{.JSON "event.n"}}|` +
			`{{.JSON "nope.x"}}|{{.Query "challenge"}}|{{.Var.TOKEN}}|{{.Var.MISSING}}|{{.Method}} {{.Path}}`}
	out, err := reply.Render(d)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out.Body); got != `3eZbrw1aB|eight|{"id":7}|1.50||q|tok_test||POST /slack/events` {
		t.Fatalf("body %q", got)
	}
	if fmt.Sprint(out.Headers) != "[{X-Echo abc} {X-N 42}]" {
		t.Fatalf("headers %v", out.Headers)
	}
	if _, err := (Reply{Body: "{{index .Body 1000}}"}).Render(d); err == nil ||
		err.Error() != "body: line 1, column 2: <index .Body 1000>: error calling index: index out of range: 1000" {
		t.Fatalf("execution error: %v", err)
	}
	if _, err := (Reply{Body: "{{.Nope}}"}).Render(d); err == nil || !strings.Contains(err.Error(), "body") {
		t.Fatalf("unknown field: %v", err)
	}
	if _, err := (Reply{Headers: []Header{{"X", "{{.Body}}"}}}).Render(NewData(Request{Body: []byte("a\r\nb")}, nil, 0)); err == nil {
		t.Fatal("a header value rendered with a line break must fail")
	}
	big := Reply{Body: `{{printf "%0999999d" 0}}{{printf "%0999999d" 0}}`}
	if _, err := big.Render(d); err == nil || !strings.Contains(err.Error(), "over 1048576 bytes") {
		t.Fatalf("a body over 1 MiB must fail: %v", err)
	}
	// Raw bytes go out exactly as given, never as a template.
	raw, err := (Reply{Status: 200, BodyBase64: base64.StdEncoding.EncodeToString([]byte("\xff\xfe{{.N}}"))}).Render(d)
	if err != nil || string(raw.Body) != "\xff\xfe{{.N}}" {
		t.Fatalf("raw body %q, %v", raw.Body, err)
	}
	notJSON := NewData(Request{Body: []byte("a=1")}, nil, 0)
	if notJSON.JSON("a") != "" {
		t.Fatal("JSON of a non-JSON body must be empty")
	}
}

func TestTextFormats(t *testing.T) {
	hs, err := ParseHeaders("Content-Type: application/json\r\n\nX-A: 1: with colon\nX-A:2\n")
	if err != nil || fmt.Sprint(hs) != "[{Content-Type application/json} {X-A 1: with colon} {X-A 2}]" {
		t.Fatalf("%v %v", hs, err)
	}
	if FormatHeaders(hs) != "Content-Type: application/json\nX-A: 1: with colon\nX-A: 2\n" {
		t.Fatalf("format %q", FormatHeaders(hs))
	}
	if _, err := ParseHeaders("no colon here"); err == nil {
		t.Fatal("a header line without a colon must fail")
	}
	vs, err := ParseVars("TOKEN=tok=x\n\n SECRET = s \n")
	if err != nil || fmt.Sprint(vs) != "[{TOKEN tok=x} {SECRET s}]" || FormatVars(vs) != "TOKEN=tok=x\nSECRET=s\n" {
		t.Fatalf("%v %v %q", vs, err, FormatVars(vs))
	}
	if _, err := ParseVars("nonsense"); err == nil {
		t.Fatal("a variable line without = must fail")
	}
}

func TestParseRoundTrip(t *testing.T) {
	if c, err := Parse(""); err != nil || c.Default.Status != 200 {
		t.Fatalf("empty: %+v %v", c, err)
	}
	c := Config{Default: Reply{Status: 418, Body: "teapot"}, Rules: []Rule{{When: Condition{Kind: WhenEvery, N: 2}, Do: ActDrop}}}
	back, err := Parse(c.String())
	if err != nil || back.String() != c.String() {
		t.Fatalf("round trip: %v %v", back, err)
	}
	if _, err := Parse("{broken"); err == nil {
		t.Fatal("broken JSON must fail")
	}
}

func TestOutcome(t *testing.T) {
	o := Outcome{Source: FromRule, Rule: 2, Label: "every 3rd", Action: ActReply, Status: 500}
	if o.Stamp() != "500 sent" || o.Why() != "rule 2 · every 3rd" || o.Good() {
		t.Fatalf("%q %q %v", o.Stamp(), o.Why(), o.Good())
	}
	if (Outcome{Action: ActTimeout}).Stamp() != "timeout" || (Outcome{Action: ActDrop}).Stamp() != "dropped" ||
		(Outcome{Action: ActRefused, Status: 401}).Stamp() != "401 refused" {
		t.Fatal("stamps")
	}
	// A body that is not UTF-8 is kept as base64, exactly; text stays text.
	var raw, text Outcome
	raw.SetReply(Rendered{Status: 200, Body: []byte{0xff, 0xfe, 'a'}})
	text.SetReply(Rendered{Status: 200, Body: []byte("héllo")})
	if raw.Body != "" || raw.BodyBase64 != "//5h" || text.Body != "héllo" || text.BodyBase64 != "" {
		t.Fatalf("raw %+v, text %+v", raw, text)
	}
	if got := ParseOutcome(raw.String(), 0); got.BodyBase64 != "//5h" {
		t.Fatalf("round trip of a raw body %+v", got)
	}
	// Cut inside a character: the text is cut before it, still text.
	var long Outcome
	long.SetReply(Rendered{Status: 200, Body: []byte(strings.Repeat("a", maxRecordedBody-1) + "é")})
	if !long.Cut || long.BodyBase64 != "" || len(long.Body) != maxRecordedBody-1 {
		t.Fatalf("cut text: cut %v, %d bytes, base64 %q", long.Cut, len(long.Body), long.BodyBase64[:min(10, len(long.BodyBase64))])
	}
	var big Outcome
	big.SetReply(Rendered{Status: 200, Body: make([]byte, maxRecordedBody+1)})
	if !big.Cut || len(big.Body) != maxRecordedBody {
		t.Fatal("recorded body not cut")
	}
	if got := ParseOutcome("", 404); got.Status != 404 || got.Action != ActReply {
		t.Fatalf("legacy outcome %+v", got)
	}
	if got := ParseOutcome(o.String(), 0); got.Rule != 2 || got.Label != "every 3rd" {
		t.Fatalf("round trip %+v", got)
	}
}

func TestDelayObeysTheCurrentLimit(t *testing.T) {
	r := Reply{DelayMs: 10_000, JitterMs: 1<<63 - 1}
	for range 100 {
		if d := r.Delay(2 * time.Second); d > 2*time.Second || d < 0 {
			t.Fatalf("delay %v over the 2 s limit", d)
		}
	}
	if d := (Reply{DelayMs: 100}).Delay(time.Minute); d != 100*time.Millisecond {
		t.Fatalf("plain delay %v", d)
	}
}

// AsSent drops what Go's HTTP server does not send.
func TestAsSent(t *testing.T) {
	r := Rendered{Status: 200, Headers: []Header{{"Content-Type", "application/json"}, {"X-A", "1"}, {"Content-Length", "11"}}, Body: []byte(`{"ok":true}`)}
	for _, c := range []struct {
		status  int
		method  string
		headers string
		body    string
		note    string
	}{
		{200, "POST", "Content-Type X-A Content-Length", `{"ok":true}`, ""},
		{200, "HEAD", "Content-Type X-A Content-Length", "", "body of 11 B not sent: the request was HEAD"},
		{204, "POST", "Content-Type X-A", "", "body of 11 B, header Content-Length not sent: a 204 reply has none"},
		{304, "GET", "X-A", "", "body of 11 B, header Content-Type, header Content-Length not sent: a 304 reply has none"},
	} {
		in := r
		in.Status = c.status
		sent, note := in.AsSent(c.method)
		var names []string
		for _, h := range sent.Headers {
			names = append(names, h.Name)
		}
		if strings.Join(names, " ") != c.headers || string(sent.Body) != c.body || note != c.note || sent.Status != c.status {
			t.Errorf("%d %s: %v %q %q", c.status, c.method, names, sent.Body, note)
		}
	}
	if _, note := (Rendered{Status: 204}).AsSent("POST"); note != "" {
		t.Errorf("nothing dropped, yet %q", note)
	}
}
