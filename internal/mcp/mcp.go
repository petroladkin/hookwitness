// Package mcp serves Hookwitness to AI agents over the Model Context
// Protocol: task-shaped tools (create an endpoint, configure it, wait for a
// request, expect verdicts) built on the REST API, called in-process, so
// validation, errors and access rules are the API's. internal/web mounts
// the Streamable HTTP handler at <prefix>/mcp, behind its host check and
// cross-origin protection; Bridge connects a stdio client to it.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/petroladkin/hookwitness/internal/access"
)

// NewServer is an MCP server whose tools call api (the REST API's handler,
// served at base) as if addressed to host. Waits end when stop is closed
// (the server is shutting down); nil never closes.
func NewServer(api http.Handler, base, host, version string, stop <-chan struct{}) *sdk.Server {
	return newServer(api, base, host, version, stop, nil)
}

func newServer(api http.Handler, base, host, version string, stop <-chan struct{}, onSession func(*sdk.ServerSession)) *sdk.Server {
	opts := &sdk.ServerOptions{
		Instructions: "Hookwitness catches webhooks for testing a sender. Create an endpoint, give its catch URL to the sender, " +
			"configure checks (signatures, tokens, JSON Schema) and fault rules, then wait_for_request or expect verdicts. " +
			"Secrets are write-only; use test values.",
	}
	if onSession != nil {
		opts.InitializedHandler = func(_ context.Context, r *sdk.InitializedRequest) { onSession(r.Session) }
	}
	srv := sdk.NewServer(&sdk.Implementation{Name: "hookwitness", Title: "Hookwitness", Version: version}, opts)
	srv.AddReceivingMiddleware(carryIdentity)
	(&tools{c: &client{api: api, base: base, host: host}, stop: stop}).register(srv)
	return srv
}

// carryIdentity puts the caller's identity, which internal/web handed to the
// SDK as the request's TokenInfo, into the context of the method, so that
// the tools' API calls act as that user.
func carryIdentity(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		if ex := req.GetExtra(); ex != nil && ex.TokenInfo != nil {
			if id, ok := ex.TokenInfo.Extra[access.MCPExtra].(*access.Identity); ok {
				ctx = access.With(ctx, id)
			}
		}
		return next(ctx, method, req)
	}
}

// Streamable serves MCP over Streamable HTTP and knows its open sessions, so
// that a shutdown can close them: an open session holds a request (the
// client's event stream, a tool waiting), and the HTTP server's graceful
// shutdown would wait for it to its timeout. It also caps how many sessions
// exist at once: a slot is taken when a session would be created and given
// back when that session ends.
type Streamable struct {
	sdk      http.Handler
	log      *slog.Logger
	limit    int // most sessions at once; 0: no limit
	mu       sync.Mutex
	sessions map[*sdk.ServerSession]struct{}
	slots    int       // sessions open or being created
	warned   time.Time // the last refusal logged
	closed   bool
}

// sessionTimeout ends a session idle this long (a seam for tests).
var sessionTimeout = 30 * time.Minute

// RetryAfter is what a client refused for the session limit is told to wait.
const RetryAfter = 30 * time.Second

// Handler serves MCP over Streamable HTTP. Each session gets a server bound
// to the host the client addressed, so catch URLs read as the client sees
// the server. At most maxSessions sessions exist at once (0: no limit).
// Waits end when stop is closed; CloseSessions then ends the sessions.
func Handler(api http.Handler, base, version string, log *slog.Logger, stop <-chan struct{}, maxSessions int) *Streamable {
	s := &Streamable{sessions: map[*sdk.ServerSession]struct{}{}, log: log, limit: maxSessions}
	s.sdk = sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		srv := newServer(api, base, r.Host, version, stop, s.track)
		if c, ok := r.Context().Value(creatingKey{}).(*creating); ok {
			c.srv = srv // the last one built for the request is the session's
		}
		return srv
	}, &sdk.StreamableHTTPOptions{
		Logger:         log,
		SessionTimeout: sessionTimeout,
		// internal/web's host check guards against DNS rebinding, -admin-hosts
		// included; the SDK's own check would refuse those names behind a
		// local proxy.
		DisableLocalhostProtection: true,
	})
	return s
}

type creatingKey struct{}

// creating is a request that would create a session: the server built for
// it, whose session holds the slot taken.
type creating struct{ srv *sdk.Server }

// ServeHTTP takes a slot for a request that would create a session (a POST
// without a session id: initialize), refusing it when none is free;
// requests of existing sessions pass untouched.
func (s *Streamable) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("Mcp-Session-Id") != "" {
		s.sdk.ServeHTTP(w, r)
		return
	}
	if !s.take() {
		s.refuse(w, r)
		return
	}
	c := &creating{}
	s.sdk.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), creatingKey{}, c)))
	// The slot goes with the session made; none made (a failed initialize)
	// gives it back now.
	held := false
	if c.srv != nil {
		for ss := range c.srv.Sessions() {
			held = true
			go func() { ss.Wait(); s.give() }()
			break
		}
	}
	if !held {
		s.give()
	}
}

func (s *Streamable) take() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limit > 0 && s.slots >= s.limit {
		return false
	}
	s.slots++
	return true
}

func (s *Streamable) give() {
	s.mu.Lock()
	s.slots--
	s.mu.Unlock()
}

// refuse answers a session over the limit: 503 with Retry-After and a
// JSON-RPC error for the request's id, logged at most once a minute.
func (s *Streamable) refuse(w http.ResponseWriter, r *http.Request) {
	msg := fmt.Sprintf("too many MCP sessions (limit %d); close one or wait for an idle one to expire", s.limit)
	s.mu.Lock()
	now := time.Now()
	logIt := now.Sub(s.warned) >= time.Minute
	if logIt {
		s.warned = now
	}
	s.mu.Unlock()
	if logIt {
		s.log.Warn("mcp: session refused: the limit is reached", "limit", s.limit)
	}
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if json.Unmarshal(body, &req) != nil || len(req.ID) == 0 {
		req.ID = json.RawMessage("null")
	}
	var out bytes.Buffer
	json.NewEncoder(&out).Encode(map[string]any{
		"jsonrpc": "2.0", "id": req.ID,
		"error": map[string]any{"code": -32000, "message": msg},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", strconv.Itoa(int(RetryAfter/time.Second)))
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write(out.Bytes())
}

func (s *Streamable) track(ss *sdk.ServerSession) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		go ss.Close() // began as the server stops
		return
	}
	s.sessions[ss] = struct{}{}
	s.mu.Unlock()
	go func() {
		ss.Wait()
		s.mu.Lock()
		delete(s.sessions, ss)
		s.mu.Unlock()
	}()
}

// Sessions is the number of open sessions.
func (s *Streamable) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// CloseSessions closes every open session and any that begins after; it
// returns when they are closed. Close the tools' stop channel first: closing
// a session waits for its running tool calls, and a wait ends on stop.
func (s *Streamable) CloseSessions() {
	s.mu.Lock()
	s.closed = true
	open := make([]*sdk.ServerSession, 0, len(s.sessions))
	for ss := range s.sessions {
		open = append(open, ss)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, ss := range open {
		wg.Add(1)
		go func() { defer wg.Done(); ss.Close() }()
	}
	wg.Wait()
}
