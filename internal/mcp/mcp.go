// Package mcp serves Hookwitness to AI agents over the Model Context
// Protocol: task-shaped tools (create an endpoint, configure it, wait for a
// request, expect verdicts) built on the REST API, called in-process, so
// validation, errors and access rules are the API's. internal/web mounts
// the Streamable HTTP handler at <prefix>/mcp, behind its host check and
// cross-origin protection; Bridge connects a stdio client to it.
package mcp

import (
	"context"
	"log/slog"
	"net/http"
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
// shutdown would wait for it to its timeout.
type Streamable struct {
	http.Handler
	mu       sync.Mutex
	sessions map[*sdk.ServerSession]struct{}
	closed   bool
}

// Handler serves MCP over Streamable HTTP. Each session gets a server bound
// to the host the client addressed, so catch URLs read as the client sees
// the server. Waits end when stop is closed; CloseSessions then ends the
// sessions.
func Handler(api http.Handler, base, version string, log *slog.Logger, stop <-chan struct{}) *Streamable {
	s := &Streamable{sessions: map[*sdk.ServerSession]struct{}{}}
	s.Handler = sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		return newServer(api, base, r.Host, version, stop, s.track)
	}, &sdk.StreamableHTTPOptions{
		Logger:         log,
		SessionTimeout: 30 * time.Minute,
		// internal/web's host check guards against DNS rebinding, -admin-hosts
		// included; the SDK's own check would refuse those names behind a
		// local proxy.
		DisableLocalhostProtection: true,
	})
	return s
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
