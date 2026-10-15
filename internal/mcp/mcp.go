// Package mcp serves Hookwitness to AI agents over the Model Context
// Protocol: task-shaped tools (create an endpoint, configure it, wait for a
// request, expect verdicts) built on the REST API, called in-process, so
// validation, errors and access rules are the API's. internal/web mounts
// the Streamable HTTP handler at <prefix>/mcp, behind its host check and
// cross-origin protection; Bridge connects a stdio client to it.
package mcp

import (
	"log/slog"
	"net/http"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewServer is an MCP server whose tools call api (the REST API's handler,
// served at base) as if addressed to host.
func NewServer(api http.Handler, base, host, version string) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: "hookwitness", Title: "Hookwitness", Version: version}, &sdk.ServerOptions{
		Instructions: "Hookwitness catches webhooks for testing a sender. Create an endpoint, give its catch URL to the sender, " +
			"configure checks (signatures, tokens, JSON Schema) and fault rules, then wait_for_request or expect verdicts. " +
			"Secrets are write-only; use test values.",
	})
	(&tools{c: &client{api: api, base: base, host: host}}).register(srv)
	return srv
}

// Handler serves MCP over Streamable HTTP. Each session gets a server bound
// to the host the client addressed, so catch URLs read as the client sees
// the server.
func Handler(api http.Handler, base, version string, log *slog.Logger) http.Handler {
	return sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		return NewServer(api, base, r.Host, version)
	}, &sdk.StreamableHTTPOptions{
		Logger:         log,
		SessionTimeout: 30 * time.Minute,
		// internal/web's host check guards against DNS rebinding, -admin-hosts
		// included; the SDK's own check would refuse those names behind a
		// local proxy.
		DisableLocalhostProtection: true,
	})
}
