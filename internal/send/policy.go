package send

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// Policy decides which targets the server may send to. The zero Policy
// allows any target except the always denied addresses; ParsePolicy reads
// -replay-allow, which narrows it to a list or turns sending off.
//
// The denied addresses are link-local networks and cloud metadata services:
// the classic targets of server-side request forgery, and nothing a webhook
// is ever replayed to. They stay denied whatever the list says. The checks
// run on the address actually dialled (CheckAddr), so a host name that
// resolves, or re-resolves, to one of them is refused too.
type Policy struct {
	off   bool
	rules []rule // empty: any target that is not denied
	text  []string
}

// rule is one -replay-allow entry: a host name, an address or a network,
// with an optional port (0: any).
type rule struct {
	name   string       // lower-case host name; "" for an address rule
	prefix netip.Prefix // valid for an address or network rule
	port   int
}

// ErrRefused wraps every refusal of the policy, so callers can tell them
// from failures of the target.
var ErrRefused = errors.New("refused by the target policy")

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, a...))
}

// denied lists the networks never dialled, with the rule a refusal names.
var denied = []struct {
	prefix netip.Prefix
	why    string
}{
	{netip.MustParsePrefix("169.254.0.0/16"), "a link-local address (169.254.0.0/16, where cloud metadata services live)"},
	{netip.MustParsePrefix("fe80::/10"), "a link-local address (fe80::/10)"},
	{netip.MustParsePrefix("fd00:ec2::254/128"), "a cloud metadata address (fd00:ec2::254)"},
	{netip.MustParsePrefix("100.100.100.200/32"), "a cloud metadata address (100.100.100.200)"},
}

// nat64 is the well-known NAT64 prefix: its addresses reach the IPv4
// address in their last 32 bits.
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// deniedWhy reports why a is never dialled, or "".
func deniedWhy(a netip.Addr) string {
	a = a.WithZone("").Unmap()
	if a.Is6() && nat64.Contains(a) {
		b := a.As16()
		a = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	if a.IsUnspecified() {
		return "an unspecified address (" + a.String() + ", which reaches this host)"
	}
	for _, d := range denied {
		if d.prefix.Contains(a) {
			return d.why
		}
	}
	return ""
}

// ParsePolicy reads -replay-allow: empty for the default (any target that
// is not denied), "none" to turn sending off, or a comma-separated list of
// host names, addresses and CIDR networks, each optionally with a port
// (host:port, [v6]:port).
func ParsePolicy(s string) (Policy, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Policy{}, nil
	}
	if strings.EqualFold(s, "none") {
		return Policy{off: true}, nil
	}
	var p Policy
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return Policy{}, errors.New("an empty entry in the list")
		}
		if strings.EqualFold(item, "none") {
			return Policy{}, errors.New(`"none" turns replay off and cannot be combined with other entries`)
		}
		r, err := parseRule(item)
		if err != nil {
			return Policy{}, fmt.Errorf("%q: %w", item, err)
		}
		p.rules = append(p.rules, r)
		p.text = append(p.text, item)
	}
	return p, nil
}

func parseRule(item string) (rule, error) {
	if strings.Contains(item, "/") {
		pre, err := netip.ParsePrefix(item)
		if err != nil {
			return rule{}, errors.New("not a CIDR network such as 198.51.100.0/24")
		}
		return rule{prefix: pre.Masked()}, nil
	}
	host, port := item, 0
	if h, ps, err := net.SplitHostPort(item); err == nil {
		n, err := strconv.Atoi(ps)
		if err != nil || n < 1 || n > 65535 {
			return rule{}, errors.New("the port is not a number from 1 to 65535")
		}
		host, port = h, n
	} else if strings.Count(item, ":") == 1 {
		return rule{}, errors.New("the port is not a number from 1 to 65535")
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return rule{prefix: netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()), port: port}, nil
	}
	if !validName(host) {
		return rule{}, errors.New("not a host name, address, host:port or CIDR network")
	}
	return rule{name: strings.ToLower(host), port: port}, nil
}

func validName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.', c == '_':
		default:
			return false
		}
	}
	return true
}

// Off reports whether sending is turned off.
func (p Policy) Off() bool { return p.off }

// String describes the policy for logs and refusals.
func (p Policy) String() string {
	switch {
	case p.off:
		return "replay is off"
	case len(p.rules) == 0:
		return "any target except link-local and cloud-metadata addresses"
	}
	return "only " + strings.Join(p.text, ", ")
}

// ParseTarget reads a target URL: http or https, a host, no user name or
// password (they would be stored with the replay), no fragment.
func ParseTarget(s string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return nil, errors.New("the URL cannot be read: " + strings.TrimPrefix(err.Error(), "parse "))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("the URL must start with http or https")
	}
	if u.User != nil {
		return nil, errors.New("the URL must not carry a user name or password: they would be stored with the replay")
	}
	if u.Hostname() == "" {
		return nil, errors.New("the URL has no host")
	}
	if u.Fragment != "" || strings.HasSuffix(s, "#") {
		return nil, errors.New("the URL must not have a #fragment: it is never sent")
	}
	if ps := u.Port(); ps != "" {
		if n, err := strconv.Atoi(ps); err != nil || n < 1 || n > 65535 {
			return nil, errors.New("the port is not a number from 1 to 65535")
		}
	}
	return u, nil
}

// Endpoint returns the host and port a target URL dials.
func Endpoint(u *url.URL) (string, int) {
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = 80
		if u.Scheme == "https" {
			port = 443
		}
	}
	return u.Hostname(), port
}

// CheckURL refuses what can be refused before resolving the host: sending
// turned off, a scheme other than http/https, credentials in the URL, and a
// literal denied address.
func (p Policy) CheckURL(u *url.URL) error {
	if p.off {
		return refuse("replay is off on this server (-replay-allow none)")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return refuse("the URL must start with http or https")
	}
	if u.User != nil {
		return refuse("the URL must not carry a user name or password")
	}
	host, _ := Endpoint(u)
	if a, err := netip.ParseAddr(host); err == nil {
		if why := deniedWhy(a); why != "" {
			return refuse("%s is %s", host, why)
		}
	}
	return nil
}

// CheckAddr decides on the address about to be dialled for host (the URL's
// host, as written) and port.
func (p Policy) CheckAddr(host string, port int, a netip.Addr) error {
	if p.off {
		return refuse("replay is off on this server (-replay-allow none)")
	}
	if why := deniedWhy(a); why != "" {
		if host != "" && host != a.String() {
			return refuse("%s resolves to %s, %s", host, a, why)
		}
		return refuse("%s is %s", a, why)
	}
	if len(p.rules) == 0 {
		return nil
	}
	ua := a.WithZone("").Unmap()
	for _, r := range p.rules {
		if r.port != 0 && r.port != port {
			continue
		}
		if r.name != "" && strings.EqualFold(r.name, host) {
			return nil
		}
		if r.prefix.IsValid() && r.prefix.Contains(ua) {
			return nil
		}
	}
	return refuse("%s is not in -replay-allow (%s)", net.JoinHostPort(host, strconv.Itoa(port)), strings.Join(p.text, ", "))
}
