package send

import (
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func TestParsePolicy(t *testing.T) {
	for _, tc := range []struct {
		in      string
		err     string // part of the error, "" for none
		summary string
	}{
		{"", "", "any target except link-local and cloud-metadata addresses"},
		{"  ", "", "any target except link-local and cloud-metadata addresses"},
		{"none", "", "replay is off"},
		{"NONE", "", "replay is off"},
		{"localhost,198.51.100.0/24, handler.test:8443 ,192.0.2.7,[::1]:3000", "", "only localhost, 198.51.100.0/24, handler.test:8443, 192.0.2.7, [::1]:3000"},
		{"none,localhost", "none", ""},
		{"198.51.100.0/33", "198.51.100.0/33", ""},
		{"host:port", "host:port", ""},
		{"a b", "a b", ""},
		{"https://x", "https://x", ""},
		{",", "empty", ""},
	} {
		p, err := ParsePolicy(tc.in)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("ParsePolicy(%q) error %v, want one naming %q", tc.in, err, tc.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParsePolicy(%q): %v", tc.in, err)
			continue
		}
		if got := p.String(); got != tc.summary {
			t.Errorf("ParsePolicy(%q).String() = %q, want %q", tc.in, got, tc.summary)
		}
	}
}

// TestDeniedAddresses: link-local and metadata addresses are refused in
// every spelling a resolver or a literal can give them, whatever the policy.
func TestDeniedAddresses(t *testing.T) {
	denied := []string{
		"169.254.169.254", "169.254.0.1", "169.254.255.255",
		"::ffff:169.254.169.254", // IPv4-mapped
		"64:ff9b::a9fe:a9fe",     // NAT64 of 169.254.169.254
		"fe80::1", "fe80::abcd:1%eth0", "febf::1",
		"fd00:ec2::254",   // AWS IMDS over IPv6
		"100.100.100.200", // Alibaba Cloud metadata
		"::ffff:100.100.100.200",
		"0.0.0.0", "::", // "this host": a way round a loopback-only allowlist
	}
	allowed := []string{"127.0.0.1", "::1", "198.51.100.3", "198.18.0.10", "100.64.0.1", "203.0.113.9", "2001:db8::1",
		"fd00:ec2::253", "::ffff:127.0.0.1", "100.100.100.201"}
	var def Policy
	for _, s := range denied {
		a := netip.MustParseAddr(s)
		if err := def.CheckAddr("", 80, a); err == nil {
			t.Errorf("%s allowed, want refused", s)
		} else if !strings.Contains(err.Error(), "link-local") && !strings.Contains(err.Error(), "metadata") && !strings.Contains(err.Error(), "unspecified") {
			t.Errorf("%s: refusal %q does not name the rule", s, err)
		}
	}
	for _, s := range allowed {
		if err := def.CheckAddr("", 80, netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s refused: %v", s, err)
		}
	}
	// An allowlist that names a denied network does not open it.
	p, err := ParsePolicy("169.254.0.0/16,fe80::/10")
	if err != nil {
		t.Fatal(err)
	}
	if p.CheckAddr("", 80, netip.MustParseAddr("169.254.169.254")) == nil {
		t.Error("allowlist opened the metadata address")
	}
}

func TestAllowlist(t *testing.T) {
	p, err := ParsePolicy("localhost,198.51.100.0/24,handler.test:8443,192.0.2.7,[::1]:3000")
	if err != nil {
		t.Fatal(err)
	}
	type tc struct {
		url  string
		addr string // the resolved address
		ok   bool
	}
	for _, c := range []tc{
		{"http://localhost:3000/x", "127.0.0.1", true}, // a name entry: any port
		{"http://LOCALHOST/x", "127.0.0.1", true},
		{"http://198.51.100.7:81/", "198.51.100.7", true},
		{"http://inside.test/", "198.51.100.5", true}, // a CIDR entry matches the resolved address
		{"https://handler.test:8443/", "203.0.113.5", true},
		{"https://handler.test/", "203.0.113.5", false}, // wrong port (443)
		{"http://192.0.2.7/", "192.0.2.7", true},
		{"http://192.0.2.8/", "192.0.2.8", false},
		{"http://[::1]:3000/", "::1", true},
		{"http://[::1]:3001/", "::1", false},
		{"http://example.test/", "203.0.113.1", false},
		{"http://localhost/", "169.254.169.254", false}, // a listed name resolving to metadata
	} {
		u, _ := url.Parse(c.url)
		host, port := Endpoint(u)
		err := p.CheckURL(u)
		if err == nil {
			err = p.CheckAddr(host, port, netip.MustParseAddr(c.addr))
		}
		if (err == nil) != c.ok {
			t.Errorf("%s at %s: err %v, want allowed=%v", c.url, c.addr, err, c.ok)
		}
		if err != nil && !c.ok && !strings.Contains(err.Error(), "-replay-allow") && !strings.Contains(err.Error(), "metadata") {
			t.Errorf("%s: refusal %q names no rule", c.url, err)
		}
	}
	off, _ := ParsePolicy("none")
	u, _ := url.Parse("http://127.0.0.1/")
	if err := off.CheckURL(u); err == nil || !strings.Contains(err.Error(), "off") {
		t.Errorf("none: %v", err)
	}
}

func TestCheckURL(t *testing.T) {
	var p Policy
	for in, want := range map[string]string{
		"http://127.0.0.1:3000/hook":      "",
		"https://handler.test/x?y=1":      "",
		"ftp://x/":                        "http or https",
		"http://user:pw@host/":            "user name or password",
		"http://user@host/":               "user name or password",
		"//host/":                         "http or https",
		"http:///nohost":                  "host",
		"http://host:0/":                  "port",
		"http://host:99999/":              "port",
		"http://169.254.169.254/latest/":  "link-local",
		"http://[fe80::1%25eth0]:80/":     "link-local",
		"http://[::ffff:169.254.169.254]": "link-local",
		"http://h/#frag":                  "fragment",
	} {
		u, err := ParseTarget(in)
		if err == nil {
			err = p.CheckURL(u)
		}
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", in, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: err %v, want one with %q", in, err, want)
		}
	}
}
