package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// MaxBody is how much of a request body a tool result carries; the result
// names the full size and where the whole body is.
const MaxBody = 16 << 10

// Waits are chained API long-polls; a call waits at most MaxWait.
const (
	DefaultWait = 30 * time.Second
	MaxWait     = 300 * time.Second
)

// apiWait is one long-poll of a chain: the API's longest. Tests shorten it
// to see the chain.
var apiWait = 60 * time.Second

// result is what every tool answers: a summary line, then compact JSON.
// The structured content (which some clients show instead of the text)
// carries the summary too.
func result(summary string, out map[string]any) (*sdk.CallToolResult, map[string]any, error) {
	b, _ := json.Marshal(out)
	structured := make(map[string]any, len(out)+1)
	for k, v := range out {
		structured[k] = v
	}
	structured["summary"] = summary
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: summary + "\n" + string(b)}}}, structured, nil
}

func ptr[T any](v T) *T { return &v }

var (
	readOnly    = &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}
	idempotent  = &sdk.ToolAnnotations{IdempotentHint: true, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}
	additive    = &sdk.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}
	destructive = &sdk.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(false)}
)

// Filters narrow the requests a tool looks at, as the API's list does.
type Filters struct {
	Verdict string   `json:"verdict,omitempty" jsonschema:"only requests with this overall verdict: pass, fail, n/v or n/c"`
	Status  *int     `json:"status,omitempty" jsonschema:"only requests answered with this status (0: nothing sent, e.g. a timeout)"`
	Text    string   `json:"text,omitempty" jsonschema:"only requests whose body contains this text (case-sensitive)"`
	JSON    []string `json:"json,omitempty" jsonschema:"only requests whose JSON body has these values, each as pointer=value, e.g. /type=order.paid; all must match"`
}

func (f Filters) query(q url.Values) url.Values {
	if f.Verdict != "" {
		q.Set("verdict", f.Verdict)
	}
	if f.Status != nil {
		q.Set("status", strconv.Itoa(*f.Status))
	}
	if f.Text != "" {
		q.Set("q", f.Text)
	}
	for _, j := range f.JSON {
		q.Add("json", j)
	}
	return q
}

func (f Filters) String() string {
	var parts []string
	if f.Verdict != "" {
		parts = append(parts, "verdict="+f.Verdict)
	}
	if f.Status != nil {
		parts = append(parts, "status="+strconv.Itoa(*f.Status))
	}
	if f.Text != "" {
		parts = append(parts, "text="+strconv.Quote(f.Text))
	}
	parts = append(parts, f.JSON...)
	return strings.Join(parts, " ")
}

// EndpointIn names an endpoint.
type EndpointIn struct {
	Endpoint string `json:"endpoint" jsonschema:"the endpoint: its id, or its exact path such as /orders/events"`
}

func (s *tools) register(srv *sdk.Server) {
	sdk.AddTool(srv, &sdk.Tool{Name: "create_endpoint", Annotations: additive,
		Description: "Create an endpoint: an exact path that catches webhooks. Returns its id and catch URL, the address to give the sender. Leave path empty for a random one."},
		s.createEndpoint)
	sdk.AddTool(srv, &sdk.Tool{Name: "list_endpoints", Annotations: readOnly,
		Description: "List the endpoints with their catch URLs and how many requests each keeps."},
		s.listEndpoints)
	sdk.AddTool(srv, &sdk.Tool{Name: "get_endpoint", Annotations: readOnly,
		Description: "One endpoint and its behaviour: checks, rules, the default reply, variables, schemas, and its secrets (names, kinds and fingerprints; values are never shown)."},
		s.getEndpoint)
	sdk.AddTool(srv, &sdk.Tool{Name: "configure_endpoint", Annotations: idempotent,
		Description: "Set parts of an endpoint's behaviour; the parts given replace those parts, the others stay. " +
			"default: the reply {status, headers:[{name,value}], body (a Go template) or body_base64 (exact bytes), delay_ms, jitter_ms}. " +
			"rules: in order, the first whose condition holds decides: {when:{kind: every|first|random, n | percent}, do: reply|timeout|drop, reply}. " +
			"checks: run first: a required header {header, match: present|equals|regex, value}, an authorization check " +
			"{kind: github|stripe|standard|hmac|bearer|basic, secret, user (basic), enforce} or a JSON Schema check {kind: schema, schema | schemas | select}. " +
			"vars: [{name, value}] for templates. schemas: [{name, text}] JSON Schemas. Secrets are set with set_secret."},
		s.configureEndpoint)
	sdk.AddTool(srv, &sdk.Tool{Name: "set_secret", Annotations: idempotent,
		Description: "Create or replace an endpoint's secret for its checks: kind key for signing keys (GitHub, Stripe, Standard Webhooks, HMAC), token for bearer tokens and Basic passwords. Write-only: the value is never returned, only a fingerprint. Use test values."},
		s.setSecret)
	sdk.AddTool(srv, &sdk.Tool{Name: "wait_for_request", Annotations: readOnly,
		Description: "Wait for the next request to an endpoint that matches the filters, and return it in full (headers in order, body, the answer, verdicts). " +
			"Without after, waits for requests that arrive after the call; with after (a request id), returns the first matching one after it. Up to timeout_s (default 30, at most 300)."},
		s.waitForRequest)
	sdk.AddTool(srv, &sdk.Tool{Name: "list_requests", Annotations: readOnly,
		Description: "List an endpoint's requests (or the Unmatched register: endpoint \"unmatched\"), newest first; with after, the newer ones oldest first. Filters as in wait_for_request."},
		s.listRequests)
	sdk.AddTool(srv, &sdk.Tool{Name: "get_request", Annotations: readOnly,
		Description: "One request in full: method, path, query, headers in the order sent, the body (cut at 16 KiB with its size), what was answered, and every verdict with its reason and schema problems."},
		s.getRequest)
	sdk.AddTool(srv, &sdk.Tool{Name: "expect", Annotations: readOnly,
		Description: "Assert that count requests matching the filters arrive at an endpoint within within_s seconds (default 30, at most 300), counting from after (default: now). " +
			"Returns pass with the matching requests, or fail with what arrived instead and the reasons of their verdicts."},
		s.expect)
	sdk.AddTool(srv, &sdk.Tool{Name: "reset_endpoint", Annotations: destructive,
		Description: "Reset an endpoint: counter starts the rules' count again (\"the first 2\" counts anew); requests deletes its kept requests."},
		s.resetEndpoint)
	sdk.AddTool(srv, &sdk.Tool{Name: "recheck", Annotations: idempotent,
		Description: "Verify an endpoint's kept requests again with its current checks, secrets and schemas, e.g. after fixing a secret. Only verdicts change."},
		s.recheck)
	sdk.AddTool(srv, &sdk.Tool{Name: "switch_endpoint", Annotations: idempotent,
		Description: "Turn an endpoint off (enabled false) or on (enabled true). Off keeps its path, behaviour, secrets, requests and counter; " +
			"requests to its path are answered 404 and kept in the Unmatched register until it is on again."},
		s.switchEndpoint)
	sdk.AddTool(srv, &sdk.Tool{Name: "delete_endpoint", Annotations: destructive,
		Description: "Delete an endpoint with its behaviour, secrets and requests."},
		s.deleteEndpoint)
}

type tools struct {
	c    *client
	stop <-chan struct{} // closed when the server shuts down: waits end
}

// ---- endpoints ---------------------------------------------------------------

type CreateIn struct {
	Path string `json:"path,omitempty" jsonschema:"the exact path, e.g. /orders/events; empty for a random one"`
}

func (s *tools) createEndpoint(ctx context.Context, _ *sdk.CallToolRequest, in CreateIn) (*sdk.CallToolResult, map[string]any, error) {
	ep, err := s.c.call(ctx, http.MethodPost, "/endpoints", nil, map[string]string{"path": in.Path})
	if err != nil {
		return nil, nil, err
	}
	return result(fmt.Sprintf("Created %v (endpoint %v): send webhooks to %v", ep["path"], num(ep["id"]), ep["url"]), ep)
}

func (s *tools) listEndpoints(ctx context.Context, _ *sdk.CallToolRequest, _ struct{}) (*sdk.CallToolResult, map[string]any, error) {
	out, err := s.c.get(ctx, "/endpoints", nil)
	if err != nil {
		return nil, nil, err
	}
	eps, _ := out["endpoints"].([]any)
	off := 0
	for _, ep := range eps {
		if m, _ := ep.(map[string]any); m["enabled"] == false {
			off++
		}
	}
	summary := fmt.Sprintf("%d endpoint(s)", len(eps))
	if off > 0 {
		summary += fmt.Sprintf(", %d off (requests to an off endpoint go to Unmatched)", off)
	}
	return result(summary, out)
}

func (s *tools) getEndpoint(ctx context.Context, _ *sdk.CallToolRequest, in EndpointIn) (*sdk.CallToolResult, map[string]any, error) {
	p, err := s.c.endpointPath(ctx, in.Endpoint)
	if err != nil {
		return nil, nil, err
	}
	if p == "/unmatched" {
		return nil, nil, errors.New("the Unmatched register has no behaviour; list_requests reads it")
	}
	ep, err := s.c.get(ctx, p, nil)
	if err != nil {
		return nil, nil, err
	}
	b, err := s.c.get(ctx, p+"/behaviour", nil)
	if err != nil {
		return nil, nil, err
	}
	ep["behaviour"] = b
	summary := fmt.Sprintf("%v (endpoint %v): %v request(s) kept", ep["path"], num(ep["id"]), num(ep["requests"]))
	if ep["enabled"] == false {
		summary += "; off: requests to it go to Unmatched"
	}
	return result(summary, ep)
}

type SwitchIn struct {
	Endpoint string `json:"endpoint" jsonschema:"the endpoint: its id or its exact path"`
	Enabled  *bool  `json:"enabled" jsonschema:"false turns it off, true on"`
}

func (s *tools) switchEndpoint(ctx context.Context, _ *sdk.CallToolRequest, in SwitchIn) (*sdk.CallToolResult, map[string]any, error) {
	if in.Enabled == nil {
		return nil, nil, errors.New("say enabled: false to turn the endpoint off, true to turn it on")
	}
	p, err := s.c.endpointPath(ctx, in.Endpoint)
	if err != nil {
		return nil, nil, err
	}
	if p == "/unmatched" {
		return nil, nil, errors.New("the Unmatched register cannot be turned off")
	}
	ep, err := s.c.call(ctx, http.MethodPatch, p, nil, map[string]bool{"enabled": *in.Enabled})
	if err != nil {
		return nil, nil, err
	}
	if *in.Enabled {
		return result(fmt.Sprintf("%v is on: it catches again with its behaviour and counter", ep["path"]), ep)
	}
	return result(fmt.Sprintf("%v is off: requests to it are answered 404 and kept in the Unmatched register", ep["path"]), ep)
}

type ConfigureIn struct {
	Endpoint string `json:"endpoint" jsonschema:"the endpoint: its id or its exact path"`
	Default  any    `json:"default,omitempty" jsonschema:"the default reply"`
	Rules    any    `json:"rules,omitempty" jsonschema:"the rules, in order (replaces all rules)"`
	Checks   any    `json:"checks,omitempty" jsonschema:"the checks, in order (replaces all checks)"`
	Vars     any    `json:"vars,omitempty" jsonschema:"template variables [{name, value}] (replaces all)"`
	Schemas  any    `json:"schemas,omitempty" jsonschema:"JSON Schemas [{name, text}] (replaces all)"`
}

func (s *tools) configureEndpoint(ctx context.Context, _ *sdk.CallToolRequest, in ConfigureIn) (*sdk.CallToolResult, map[string]any, error) {
	p, err := s.c.endpointPath(ctx, in.Endpoint)
	if err != nil {
		return nil, nil, err
	}
	parts := map[string]any{"default": in.Default, "rules": in.Rules, "checks": in.Checks, "vars": in.Vars, "schemas": in.Schemas}
	var changed []string
	for _, k := range []string{"default", "rules", "checks", "vars", "schemas"} {
		if parts[k] != nil {
			changed = append(changed, k)
		}
	}
	if len(changed) == 0 {
		return nil, nil, errors.New("give at least one of default, rules, checks, vars, schemas")
	}
	// Read, change the parts given, write with If-Match; a change made in
	// between (the UI, another agent) is retried once.
	for attempt := 0; ; attempt++ {
		hd, b, err := s.c.do(ctx, http.MethodGet, p+"/behaviour", nil, nil)
		if err != nil {
			return nil, nil, err
		}
		doc := map[string]any{}
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, nil, err
		}
		delete(doc, "secrets")
		for _, k := range changed {
			doc[k] = parts[k]
		}
		out, err := s.c.putBehaviour(ctx, p, hd.Get("ETag"), doc)
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusPreconditionFailed && attempt == 0 {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		return result("Set "+strings.Join(changed, ", ")+"; the next request is answered by the new behaviour", out)
	}
}

func (c *client) putBehaviour(ctx context.Context, p, etag string, doc map[string]any) (map[string]any, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+p+"/behaviour", strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	req.Host, req.RemoteAddr = c.host, "127.0.0.1:0"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", etag)
	rec := &recorder{header: http.Header{}}
	c.api.ServeHTTP(rec, req)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	if rec.status >= 400 {
		var e struct {
			Error apiError `json:"error"`
		}
		json.Unmarshal(rec.body.Bytes(), &e)
		e.Error.Status = rec.status
		return nil, &e.Error
	}
	out := map[string]any{}
	err = json.Unmarshal(rec.body.Bytes(), &out)
	return out, err
}

type SecretIn struct {
	Endpoint string `json:"endpoint" jsonschema:"the endpoint: its id or its exact path"`
	Name     string `json:"name" jsonschema:"the secret's name, letters, digits and _ (a check refers to it)"`
	Kind     string `json:"kind" jsonschema:"key for signing keys, token for bearer tokens and Basic passwords"`
	Value    string `json:"value" jsonschema:"the value; never returned or logged"`
}

func (s *tools) setSecret(ctx context.Context, _ *sdk.CallToolRequest, in SecretIn) (*sdk.CallToolResult, map[string]any, error) {
	p, err := s.c.endpointPath(ctx, in.Endpoint)
	if err != nil {
		return nil, nil, err
	}
	out, err := s.c.call(ctx, http.MethodPut, p+"/secrets/"+url.PathEscape(in.Name), nil, map[string]string{"kind": in.Kind, "value": in.Value})
	if err != nil {
		return nil, nil, err
	}
	return result(fmt.Sprintf("Secret %v set (%v, fingerprint %v)", out["name"], out["kind"], out["fingerprint"]), out)
}

type ResetIn struct {
	Endpoint string `json:"endpoint" jsonschema:"the endpoint: its id or its exact path"`
	Counter  bool   `json:"counter,omitempty" jsonschema:"start the rules' count again"`
	Requests bool   `json:"requests,omitempty" jsonschema:"delete the kept requests"`
}

func (s *tools) resetEndpoint(ctx context.Context, _ *sdk.CallToolRequest, in ResetIn) (*sdk.CallToolResult, map[string]any, error) {
	if !in.Counter && !in.Requests {
		return nil, nil, errors.New("say what to reset: counter, requests or both")
	}
	p, err := s.c.endpointPath(ctx, in.Endpoint)
	if err != nil {
		return nil, nil, err
	}
	var done []string
	if in.Counter {
		if _, err := s.c.call(ctx, http.MethodPost, p+"/reset-counter", nil, nil); err != nil {
			return nil, nil, err
		}
		done = append(done, "counter reset")
	}
	if in.Requests {
		if _, err := s.c.call(ctx, http.MethodDelete, p+"/requests", nil, nil); err != nil {
			return nil, nil, err
		}
		done = append(done, "requests cleared")
	}
	return result(strings.Join(done, ", "), map[string]any{"counter": in.Counter, "requests": in.Requests})
}

func (s *tools) recheck(ctx context.Context, _ *sdk.CallToolRequest, in EndpointIn) (*sdk.CallToolResult, map[string]any, error) {
	p, err := s.c.endpointPath(ctx, in.Endpoint)
	if err != nil {
		return nil, nil, err
	}
	out, err := s.c.call(ctx, http.MethodPost, p+"/recheck", nil, nil)
	if err != nil {
		return nil, nil, err
	}
	return result(fmt.Sprintf("Re-checked %v request(s)", num(out["rechecked"])), out)
}

func (s *tools) deleteEndpoint(ctx context.Context, _ *sdk.CallToolRequest, in EndpointIn) (*sdk.CallToolResult, map[string]any, error) {
	p, err := s.c.endpointPath(ctx, in.Endpoint)
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.c.call(ctx, http.MethodDelete, p, nil, nil); err != nil {
		return nil, nil, err
	}
	return result("Deleted endpoint "+strings.TrimPrefix(p, "/endpoints/"), map[string]any{"deleted": in.Endpoint})
}

// ---- requests ----------------------------------------------------------------

type ListIn struct {
	Endpoint string `json:"endpoint" jsonschema:"the endpoint: its id, its exact path, or unmatched"`
	Filters
	After       *int64 `json:"after,omitempty" jsonschema:"only requests newer than this request id, oldest first"`
	Before      *int64 `json:"before,omitempty" jsonschema:"only requests older than this request id (paging back)"`
	Limit       int    `json:"limit,omitempty" jsonschema:"at most this many (default 50)"`
	IncludeBody bool   `json:"include_body,omitempty" jsonschema:"carry each request's body (cut at 16 KiB)"`
}

func (s *tools) listRequests(ctx context.Context, _ *sdk.CallToolRequest, in ListIn) (*sdk.CallToolResult, map[string]any, error) {
	p, err := s.c.endpointPath(ctx, in.Endpoint)
	if err != nil {
		return nil, nil, err
	}
	q := in.Filters.query(url.Values{})
	if in.After != nil {
		q.Set("after", strconv.FormatInt(*in.After, 10))
	}
	if in.Before != nil {
		q.Set("before", strconv.FormatInt(*in.Before, 10))
	}
	if in.Limit > 0 {
		q.Set("limit", strconv.Itoa(in.Limit))
	}
	if in.IncludeBody {
		q.Set("include", "body")
	}
	out, err := s.c.get(ctx, p+"/requests", q)
	if err != nil {
		return nil, nil, err
	}
	reqs, _ := out["requests"].([]any)
	for _, r := range reqs {
		s.cut(r.(map[string]any))
	}
	return result(fmt.Sprintf("%d request(s)", len(reqs)), out)
}

type GetIn struct {
	ID int64 `json:"id" jsonschema:"the request id"`
}

func (s *tools) getRequest(ctx context.Context, _ *sdk.CallToolRequest, in GetIn) (*sdk.CallToolResult, map[string]any, error) {
	r, err := s.c.get(ctx, "/requests/"+strconv.FormatInt(in.ID, 10), nil)
	if err != nil {
		return nil, nil, err
	}
	s.cut(r)
	return result(describe(r), r)
}

type WaitIn struct {
	Endpoint string `json:"endpoint" jsonschema:"the endpoint: its id or its exact path"`
	Filters
	After    *int64 `json:"after,omitempty" jsonschema:"a request id: return the first matching request after it; default: wait for requests arriving after this call"`
	TimeoutS int    `json:"timeout_s,omitempty" jsonschema:"seconds to wait, default 30, at most 300"`
}

func (s *tools) waitForRequest(ctx context.Context, _ *sdk.CallToolRequest, in WaitIn) (*sdk.CallToolResult, map[string]any, error) {
	p, err := s.c.endpointPath(ctx, in.Endpoint)
	if err != nil {
		return nil, nil, err
	}
	d, err := seconds(in.TimeoutS)
	if err != nil {
		return nil, nil, err
	}
	after, err := s.cursor(ctx, p, in.After)
	if err != nil {
		return nil, nil, err
	}
	found, err := s.waitMatching(ctx, p, in.Filters, after, 1, time.Now().Add(d))
	if err != nil {
		return nil, nil, err
	}
	if len(found) == 0 {
		return result(fmt.Sprintf("No matching request within %s (after request %d)", d, after),
			map[string]any{"arrived": false, "after": after, "waited_s": d.Seconds()})
	}
	id := int64(num(found[0]["id"]))
	r, err := s.c.get(ctx, "/requests/"+strconv.FormatInt(id, 10), nil)
	if err != nil {
		return nil, nil, err
	}
	s.cut(r)
	r["arrived"] = true
	return result(describe(r), r)
}

type ExpectIn struct {
	Endpoint string `json:"endpoint" jsonschema:"the endpoint: its id or its exact path"`
	Filters
	Count   int    `json:"count,omitempty" jsonschema:"how many matching requests must arrive, default 1"`
	After   *int64 `json:"after,omitempty" jsonschema:"count requests after this request id; default: those arriving after this call"`
	WithinS int    `json:"within_s,omitempty" jsonschema:"seconds to wait for them, default 30, at most 300"`
}

func (s *tools) expect(ctx context.Context, _ *sdk.CallToolRequest, in ExpectIn) (*sdk.CallToolResult, map[string]any, error) {
	p, err := s.c.endpointPath(ctx, in.Endpoint)
	if err != nil {
		return nil, nil, err
	}
	d, err := seconds(in.WithinS)
	if err != nil {
		return nil, nil, err
	}
	count := max(in.Count, 1)
	after, err := s.cursor(ctx, p, in.After)
	if err != nil {
		return nil, nil, err
	}
	matched, err := s.waitMatching(ctx, p, in.Filters, after, count, time.Now().Add(d))
	if err != nil {
		return nil, nil, err
	}
	want := fmt.Sprintf("%d request(s)", count)
	if f := in.Filters.String(); f != "" {
		want += " with " + f
	}
	out := map[string]any{"expected": want, "after": after, "matched": brief(matched)}
	if len(matched) >= count {
		out["result"] = "pass"
		return result(fmt.Sprintf("pass: %s arrived within %s", want, d), out)
	}
	// What arrived instead, with the reasons of its verdicts.
	all, err := s.c.get(ctx, p+"/requests", url.Values{"after": {strconv.FormatInt(after, 10)}, "limit": {"50"}})
	if err != nil {
		return nil, nil, err
	}
	isMatch := map[float64]bool{}
	for _, m := range matched {
		isMatch[num(m["id"])] = true
	}
	var others []map[string]any
	reqs, _ := all["requests"].([]any)
	for _, r := range reqs {
		rm := r.(map[string]any)
		if isMatch[num(rm["id"])] {
			continue
		}
		o := map[string]any{"id": rm["id"], "no": rm["no"], "status": rm["status"], "verdict": rm["verdict"]}
		if len(others) < 10 && rm["verdict"] != "" && rm["verdict"] != "pass" {
			if full, err := s.c.get(ctx, "/requests/"+strconv.FormatInt(int64(num(rm["id"])), 10), nil); err == nil {
				o["reasons"] = reasons(full)
			}
		}
		others = append(others, o)
	}
	out["result"] = "fail"
	out["arrived"] = len(reqs)
	out["others"] = others
	summary := fmt.Sprintf("fail: expected %s within %s; %d arrived, %d matched", want, d, len(reqs), len(matched))
	if len(reqs) == 0 {
		summary = fmt.Sprintf("fail: expected %s within %s; nothing arrived", want, d)
	}
	for i, o := range others {
		if i == 3 {
			summary += "; …"
			break
		}
		summary += fmt.Sprintf("; #%v %v %v", o["no"], verdictOr(o["verdict"]), strings.Join(stringsOf(o["reasons"]), ", "))
	}
	return result(summary, out)
}

// waitMatching waits until n requests after the cursor match the filters,
// or the deadline passes; it returns those found (oldest first). Each step
// is one API long-poll of at most a minute; the call's cancellation ends it.
func (s *tools) waitMatching(ctx context.Context, p string, f Filters, after int64, n int, deadline time.Time) ([]map[string]any, error) {
	var found []map[string]any
	cursor := after
	for len(found) < n {
		// At shutdown the API answers every wait at once: stop instead of
		// asking again until the deadline.
		select {
		case <-s.stop:
			return found, &stoppingError{matched: len(found), want: n}
		default:
		}
		left := time.Until(deadline).Round(time.Millisecond)
		q := f.query(url.Values{"after": {strconv.FormatInt(cursor, 10)}, "limit": {strconv.Itoa(n - len(found))}})
		if left > 0 {
			q.Set("wait", min(left, apiWait).String())
		}
		out, err := s.c.get(ctx, p+"/requests", q)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("cancelled: %w", ctx.Err())
			}
			return nil, err
		}
		reqs, _ := out["requests"].([]any)
		for _, r := range reqs {
			rm := r.(map[string]any)
			found = append(found, rm)
			cursor = int64(num(rm["id"]))
		}
		if len(found) >= n || time.Until(deadline) <= 0 {
			break
		}
	}
	return found, nil
}

// stoppingError ends a wait when the server shuts down.
type stoppingError struct{ matched, want int }

func (e *stoppingError) Error() string {
	return fmt.Sprintf("the server is shutting down; %d of %d matched so far; call again after it restarts", e.matched, e.want)
}

// cursor is the request id to wait after: the one given, or the newest now.
func (s *tools) cursor(ctx context.Context, p string, after *int64) (int64, error) {
	if after != nil {
		return *after, nil
	}
	out, err := s.c.get(ctx, p+"/requests", url.Values{"limit": {"1"}})
	if err != nil {
		return 0, err
	}
	if reqs, _ := out["requests"].([]any); len(reqs) > 0 {
		return int64(num(reqs[0].(map[string]any)["id"])), nil
	}
	return 0, nil
}

// cut keeps at most MaxBody of a request's body, saying the full size and
// where the whole body is.
func (s *tools) cut(r map[string]any) {
	for _, k := range []string{"body_text", "body_base64"} {
		v, ok := r[k].(string)
		if !ok || len(v) <= MaxBody {
			continue
		}
		limit := MaxBody
		if k == "body_text" {
			for limit > 0 && !utf8Start(v[limit]) {
				limit--
			}
		}
		r[k] = v[:limit]
		r["body_cut"] = true
		r["body_url"] = "http://" + s.c.host + s.c.base + "/requests/" + strconv.FormatInt(int64(num(r["id"])), 10) + "/body"
	}
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func seconds(s int) (time.Duration, error) {
	if s == 0 {
		return DefaultWait, nil
	}
	d := time.Duration(s) * time.Second
	if d < 0 || d > MaxWait {
		return 0, fmt.Errorf("wait between 1 and %d seconds", int(MaxWait.Seconds()))
	}
	return d, nil
}

func num(v any) float64 { f, _ := v.(float64); return f }

func brief(rs []map[string]any) []map[string]any {
	out := []map[string]any{}
	for _, r := range rs {
		out = append(out, map[string]any{"id": r["id"], "no": r["no"], "status": r["status"], "verdict": r["verdict"]})
	}
	return out
}

func reasons(r map[string]any) []string {
	var out []string
	vs, _ := r["verdicts"].([]any)
	for _, v := range vs {
		vm := v.(map[string]any)
		if vm["result"] != "pass" {
			out = append(out, fmt.Sprintf("%v: %v", vm["label"], vm["reason"]))
		}
	}
	return out
}

func stringsOf(v any) []string { s, _ := v.([]string); return s }

func verdictOr(v any) string {
	if s, _ := v.(string); s != "" {
		return s
	}
	return "no verdict"
}

// describe is a request's summary line: "#3 POST /hooks answered 200, verdict fail (GitHub signature: signature does not match)".
func describe(r map[string]any) string {
	s := fmt.Sprintf("#%v %v %v", num(r["no"]), r["method"], r["path"])
	if out, ok := r["outcome"].(map[string]any); ok && out["action"] != nil && out["action"] != "reply" {
		s += fmt.Sprintf(" %v", out["action"])
	} else {
		s += fmt.Sprintf(" answered %v", num(r["status"]))
	}
	if v, _ := r["verdict"].(string); v != "" {
		s += ", verdict " + v
		if rs := reasons(r); len(rs) > 0 {
			s += " (" + strings.Join(rs, "; ") + ")"
		}
	}
	return s
}
