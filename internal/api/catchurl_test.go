package api

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

func TestCatchURL(t *testing.T) {
	for _, c := range []struct {
		name  string
		cu    CatchURL
		host  string // the Host the admin was reached by
		https bool
		want  string
	}{
		{"shared: as the browser addressed us", CatchURL{Addr: "127.0.0.1:8080"}, "localhost:8080", false, "http://localhost:8080"},
		{"shared over https", CatchURL{Addr: "127.0.0.1:8080"}, "hooks.example.com", true, "https://hooks.example.com"},
		{"public URL wins, shared", CatchURL{Public: "https://hooks.example.com", Addr: "127.0.0.1:8080"}, "127.0.0.1:8080", false, "https://hooks.example.com"},
		{"public URL wins, separate", CatchURL{Public: "http://127.0.0.1:9000/hooks", Addr: "127.0.0.1:8080", Separate: true}, "127.0.0.1:8081", false, "http://127.0.0.1:9000/hooks"},
		{"separate, a specific catch address", CatchURL{Addr: "127.0.0.1:8080", Separate: true}, "127.0.0.1:8081", false, "http://127.0.0.1:8080"},
		{"separate, all interfaces: the admin's host, the catch port", CatchURL{Addr: "[::]:8080", Separate: true}, "192.0.2.7:8081", false, "http://192.0.2.7:8080"},
		{"separate, empty host", CatchURL{Addr: ":8080", Separate: true}, "localhost:8081", false, "http://localhost:8080"},
		{"separate, IPv6 admin host", CatchURL{Addr: "0.0.0.0:8080", Separate: true}, "[2001:db8::1]:8081", false, "http://[2001:db8::1]:8080"},
		{"separate, admin host without a port", CatchURL{Addr: ":8080", Separate: true}, "hooks.example.com", false, "http://hooks.example.com:8080"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = c.host
		if c.https {
			r.TLS = &tls.ConnectionState{}
		}
		if got := c.cu.Base(r); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
