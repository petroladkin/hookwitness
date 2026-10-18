package api

import (
	"net"
	"net/http"
)

// CatchURL says where senders reach the catch listener, for the URLs the
// UI, the API and MCP show.
type CatchURL struct {
	Public   string // -public-url (no trailing "/"); wins when set
	Addr     string // the catch listener's address, as listening (a real port)
	Separate bool   // the admin has its own listener
}

// Base is the catch URL without a path: the public URL when set; with the
// admin on the catch listener, how the browser (or client) addressed us;
// with the admin elsewhere, the catch listener's host when it names one,
// else the host the admin was reached by, with the catch port.
func (c CatchURL) Base(r *http.Request) string {
	if c.Public != "" {
		return c.Public
	}
	if !c.Separate {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		return scheme + "://" + r.Host
	}
	host, port, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return "http://" + c.Addr
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
	}
	return "http://" + net.JoinHostPort(host, port)
}
