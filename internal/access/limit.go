package access

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// The login limit: failures per client address in a sliding window.
const (
	MaxFailures   = 10
	FailureWindow = 15 * time.Minute
	maxTracked    = 10000 // addresses remembered; the oldest go first
)

// Limiter counts failed logins per client address. It lives in memory: a
// restart forgets it.
type Limiter struct {
	now func() time.Time
	mu  sync.Mutex
	// fails holds each address's failure times, oldest first; order lists
	// the addresses by their first tracked failure, for eviction.
	fails map[string][]time.Time
	order []string
}

// NewLimiter returns a limiter; now is the clock (nil: time.Now).
func NewLimiter(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{now: now, fails: map[string][]time.Time{}}
}

// recent drops the failures of addr older than the window; the lock is held.
func (l *Limiter) recent(addr string, now time.Time) []time.Time {
	fs := l.fails[addr]
	i := 0
	for i < len(fs) && now.Sub(fs[i]) >= FailureWindow {
		i++
	}
	fs = fs[i:]
	if len(fs) == 0 {
		delete(l.fails, addr)
	} else {
		l.fails[addr] = fs
	}
	return fs
}

// Allow reports whether addr may try to log in now; when not, wait is how
// long until it may.
func (l *Limiter) Allow(addr string) (ok bool, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	fs := l.recent(addr, now)
	if len(fs) < MaxFailures {
		return true, 0
	}
	return false, fs[len(fs)-MaxFailures].Add(FailureWindow).Sub(now)
}

// Fail records a failed login from addr.
func (l *Limiter) Fail(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if _, known := l.fails[addr]; !known {
		for len(l.fails) >= maxTracked && len(l.order) > 0 {
			delete(l.fails, l.order[0])
			l.order = l.order[1:]
		}
		l.order = append(l.order, addr)
	}
	l.fails[addr] = append(l.recent(addr, now), now)
	if len(l.order) > 2*maxTracked { // drop names of addresses already forgotten
		kept := l.order[:0]
		for _, a := range l.order {
			if _, ok := l.fails[a]; ok {
				kept = append(kept, a)
			}
		}
		l.order = kept
	}
}

// Success clears addr's failures.
func (l *Limiter) Success(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, addr)
}

// Proxies are the reverse proxies whose X-Forwarded-For is believed.
type Proxies []netip.Prefix

// ParseProxies reads a comma-separated list of addresses and CIDRs.
func ParseProxies(s string) (Proxies, error) {
	var out Proxies
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf("%q is neither an IP address nor a CIDR", part)
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return out, nil
}

func (ps Proxies) trusted(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientAddr is the address a request came from: the TCP peer, or, when the
// peer is a trusted proxy, the rightmost address of X-Forwarded-For that is
// not one (a client can prepend anything; only the proxies' entries count).
func ClientAddr(r *http.Request, ps Proxies) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || len(ps) == 0 || !ps.trusted(peer) {
		return host
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return host // a damaged header: fall back to the peer
		}
		if !ps.trusted(a) {
			return a.Unmap().String()
		}
	}
	return host
}
