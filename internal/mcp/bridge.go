package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Endpoint is the MCP URL of a server: <url>/admin/mcp for a bare server
// URL, <url>/mcp for one that names the admin prefix, or the URL itself
// when it already ends in /mcp.
func Endpoint(server string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(server))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("--url %q: give the server's URL, e.g. http://127.0.0.1:8080", server)
	}
	switch p := strings.TrimRight(u.Path, "/"); {
	case strings.HasSuffix(p, "/mcp"):
		u.Path = p
	case p == "":
		u.Path = "/admin/mcp"
	default:
		u.Path = p + "/mcp"
	}
	return u.String(), nil
}

// Bridge connects an MCP client speaking stdio (newline-delimited JSON-RPC
// on in and out) to the server's Streamable HTTP endpoint, passing every
// message through unchanged: it has no tools of its own. It returns when
// either side closes.
func Bridge(ctx context.Context, endpoint string, client *http.Client, in io.ReadCloser, out io.WriteCloser) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	local, err := (&sdk.IOTransport{Reader: in, Writer: out}).Connect(ctx)
	if err != nil {
		return err
	}
	defer local.Close()
	remote, err := (&sdk.StreamableClientTransport{Endpoint: endpoint, HTTPClient: client}).Connect(ctx)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", endpoint, err)
	}
	defer remote.Close()
	pump := func(from, to sdk.Connection) error {
		for {
			msg, err := from.Read(ctx)
			if err != nil {
				return err
			}
			if err := to.Write(ctx, msg); err != nil {
				return err
			}
		}
	}
	errs := make(chan error, 2)
	go func() { errs <- pump(local, remote) }()
	go func() { errs <- pump(remote, local) }()
	err = <-errs
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return nil // the client closed stdin, or the process is stopping
	}
	if strings.Contains(err.Error(), http.StatusText(http.StatusUnauthorized)) {
		return fmt.Errorf("%w (the server has login on and wants a token: pass -token-file with a token from its account page)", err)
	}
	if strings.Contains(err.Error(), "sending \"initialize\": "+http.StatusText(http.StatusServiceUnavailable)) {
		return fmt.Errorf("%w (the server's MCP session limit, -mcp-max-sessions, is reached: close another session or try again in %s)", err, RetryAfter)
	}
	return notFoundHint(endpoint, err)
}

// WithToken is client sending Authorization: Bearer token with every
// request to the origin of server (scheme, host and port), for a server
// with login on. A redirect to another origin goes without the token: a
// server behind a sign-in gate, or a mistyped URL, must not receive it.
func WithToken(client *http.Client, server, token string) (*http.Client, error) {
	u, err := url.Parse(server)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("the server URL %q has no scheme or host", server)
	}
	c := *client
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = bearerTransport{base: base, origin: origin(u), value: "Bearer " + token}
	return &c, nil
}

type bearerTransport struct {
	base   http.RoundTripper
	origin string
	value  string
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if origin(r.URL) != t.origin {
		return t.base.RoundTrip(r)
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", t.value)
	return t.base.RoundTrip(r)
}

// origin is a URL's scheme and host with its port made explicit, so that
// http://h and http://h:80 are one origin.
func origin(u *url.URL) string {
	scheme, host, port := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// notFoundHint adds to a "Not Found" from a server's /admin/mcp the URL an
// admin at the root of its own port has: <server>/mcp.
func notFoundHint(endpoint string, err error) error {
	u, perr := url.Parse(endpoint)
	if perr != nil || !strings.HasSuffix(u.Path, "/admin/mcp") || !strings.Contains(err.Error(), http.StatusText(http.StatusNotFound)) {
		return err
	}
	u.Path = strings.TrimSuffix(u.Path, "/admin/mcp") + "/mcp"
	return fmt.Errorf("%w (%s has no MCP there; if the admin is at the root of its own address, give -url %s)", err, endpoint, u)
}
