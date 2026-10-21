package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/api"
)

// A login's next page and a form's back page stay on this admin, with the
// admin under a prefix and at the root of its own port: a browser drops
// tabs and newlines from a URL, so "/\t/evil.example" is another site.
func TestNextPageStaysHere(t *testing.T) {
	for _, prefix := range []string{"/admin", ""} {
		h := &Handler{prefix: prefix}
		for _, next := range []string{"/", "/e/1", "/e/1/r/2?replay", "/users?saved=role"} {
			if got := h.nextPage(prefix + next); got != prefix+next {
				t.Errorf("prefix %q: %q went to %q", prefix, prefix+next, got)
			}
		}
		for _, next := range []string{
			"/\t/evil.example", "/\n/evil.example", "/\r/evil.example", "/\x7f/evil.example",
			"//evil.example", "/\\evil.example", "/\\/evil.example", "https://evil.example/",
			"http:/evil.example", "evil.example", "", prefix + "/\t/evil.example", prefix + "/x\x00y",
			prefix + "/login", prefix + "/logout",
		} {
			if got := h.nextPage(next); got != prefix+"/" {
				t.Errorf("prefix %q: %q went to %q", prefix, next, got)
			}
		}
	}
}

// The login cookie is Secure when the browser reaches this listener over
// TLS: directly, through a trusted proxy that says so, or (an https public
// URL) on the shared listener or by a name of -admin-hosts. An admin of its
// own reached by an address over plain HTTP gets a plain cookie, else the
// browser would drop it and the login would loop.
func TestSecureCookie(t *testing.T) {
	proxy, _ := access.ParseProxies("127.0.0.9")
	req := func(host, peer, proto string) *http.Request {
		r := httptest.NewRequest("POST", "http://"+host+"/login", nil)
		r.RemoteAddr = peer + ":5000"
		if proto != "" {
			r.Header.Set("X-Forwarded-Proto", proto)
		}
		return r
	}
	for _, c := range []struct {
		public   string
		separate bool
		host     string
		peer     string
		proto    string
		want     bool
	}{
		{"", false, "127.0.0.1:8080", "127.0.0.1", "", false},
		{"https://hooks.example.com", false, "127.0.0.1:8080", "127.0.0.1", "", true},
		{"https://hooks.example.com", true, "127.0.0.1:8081", "127.0.0.1", "", false},
		{"https://hooks.example.com", true, "admin.example.com", "127.0.0.9", "", true},
		{"http://hooks.example.com", true, "admin.example.com", "127.0.0.9", "https", true},
		{"http://hooks.example.com", true, "127.0.0.1:8081", "127.0.0.1", "https", false},
		{"https://hooks.example.com", true, "other.example.com", "127.0.0.1", "", false},
	} {
		h := &Handler{catch: api.CatchURL{Public: c.public, Separate: c.separate}, proxies: proxy, hosts: map[string]bool{"admin.example.com": true}}
		if got := h.secure(req(c.host, c.peer, c.proto)); got != c.want {
			t.Errorf("public %q separate %v host %s peer %s proto %q: secure %v", c.public, c.separate, c.host, c.peer, c.proto, got)
		}
	}
}
