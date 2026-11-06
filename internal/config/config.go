// Package config resolves the settings of `hookwitness serve` from flags and
// HOOKWITNESS_* environment variables. A flag given on the command line wins
// over the variable; the variable wins over the default.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/send"
)

// Config is the resolved configuration of the server.
type Config struct {
	Addr        string // listen address, host:port
	DBPath      string // sqlite file
	LogFormat   string // "text" or "json"
	MaxBody     int64  // largest request body kept, in bytes
	MaxInFlight int64  // bytes of request bodies held at once; over it requests get 503
	// MaxDBSize is the database size limit: over it the oldest bodies,
	// then requests, are dropped (pinned never); 0: none.
	MaxDBSize int64
	Retention int // normal requests kept per register by default
	// RetentionAnomalies is the anomalies kept per endpoint by default (not
	// answered 2xx, a failed verdict, a redelivery), apart from the normal
	// ones; 0: no anomaly lane, every request counts against Retention.
	RetentionAnomalies int
	AdminAddr          string        // the admin's own listen address; "" = under the prefix on Addr
	AdminPrefix        string        // path prefix of the web UI, e.g. /admin; "" = the root of AdminAddr
	PublicURL          string        // base of the catch URLs shown, e.g. https://hooks.example.com; "" = derived
	AdminHosts         []string      // host names the UI answers to besides IPs and localhost
	MaxWait            time.Duration // longest reply delay or timeout hold
	Import             string        // a setup file applied at every start (creates or replaces its endpoints)
	// TrustedProxies are the reverse proxies whose X-Forwarded-For names the
	// client (the login limit and the logs); none: the TCP peer.
	TrustedProxies access.Proxies
	// ReplayAllow is where caught requests may be replayed to: by default
	// anywhere but link-local and cloud-metadata addresses.
	ReplayAllow send.Policy
	// MCPMaxSessions caps the MCP sessions open at once; 0: no limit.
	MCPMaxSessions int
	// AdminURL is the admin's external origin (-admin-url), for the OpenID
	// Connect redirect URI; "" when not set (see AdminOrigin).
	AdminURL string
	// OIDC are the OpenID Connect providers people may sign in with.
	OIDC []Provider
}

// Provider is an OpenID Connect provider from the environment
// (HOOKWITNESS_OIDC_<NAME>_*). The client secret is read from SecretFile at
// start and kept in memory only.
type Provider struct {
	Name           string // lower-case, 1-32 of a-z 0-9
	Env            string // the variables' prefix, HOOKWITNESS_OIDC_<NAME>_
	Issuer         string
	ClientID       string
	SecretFile     string
	Label          string   // after "Sign in with"
	AllowedDomains []string // lower-case; new identities of these become users
}

// AdminOrigin is the admin's external origin: -admin-url, or the public
// URL when the admin shares its listener and the URL has no path; "" when
// neither (provider sign-in then refuses to start). Never the request's
// Host: a provider holds one registered redirect URI.
func (c Config) AdminOrigin() string {
	if c.AdminURL != "" {
		return c.AdminURL
	}
	if c.AdminAddr == "" && c.PublicURL != "" {
		if u, err := url.Parse(c.PublicURL); err == nil && (u.Path == "" || u.Path == "/") {
			return u.Scheme + "://" + u.Host
		}
	}
	return ""
}

// RedirectURI is where a provider sends the browser back.
func (c Config) RedirectURI(p Provider) string {
	return c.AdminOrigin() + c.AdminPrefix + "/oidc/" + p.Name + "/callback"
}

// Defaults.
const (
	DefaultAddr        = "127.0.0.1:8080"
	DefaultDBPath      = "hookwitness.db"
	DefaultLogFormat   = "text"
	DefaultMaxBody     = "10MiB"
	DefaultRetention   = "1000"
	DefaultAnomalies   = "1000"
	DefaultAdminPrefix = "/admin"
	DefaultMaxWait     = "5m"
	DefaultMCPSessions = "32"

	// MaxBodyLimit is the largest -max-body: sqlite stores a value of at
	// most about 1e9 bytes, and a body is read whole before it is stored.
	MaxBodyLimit = 512 << 20
	// minInFlight is the default -max-inflight unless twice -max-body is
	// more.
	// MinDBSize is the smallest -max-db-size: room for a few large bodies.
	MinDBSize   = 64 << 20
	minInFlight = 256 << 20
)

type setting struct {
	flag, env, def, usage string
	dst                   *string
}

// Parse reads args (without the subcommand name) and the environment through
// getenv. Usage and errors from the flag package go to out.
func Parse(args []string, getenv func(string) string, out io.Writer) (Config, error) {
	var c Config
	var maxBody, maxInFlight, maxDBSize, retention, anomalies, adminHosts, maxWait, proxies, replayAllow, mcpSessions, oidcProviders string
	settings := []setting{
		{"addr", "HOOKWITNESS_ADDR", DefaultAddr, "listen address (host:port)", &c.Addr},
		{"db", "HOOKWITNESS_DB", DefaultDBPath, "path to the sqlite database file", &c.DBPath},
		{"log-format", "HOOKWITNESS_LOG_FORMAT", DefaultLogFormat, "log format: text or json", &c.LogFormat},
		{"max-body", "HOOKWITNESS_MAX_BODY", DefaultMaxBody, "largest request body to keep, in bytes or with KiB/MiB/GiB; a larger one is answered 413", &maxBody},
		{"max-inflight", "HOOKWITNESS_MAX_INFLIGHT", "", "bytes of request bodies held at once, with KiB/MiB/GiB; over it a request is answered 503 (default: the larger of 256MiB and twice max-body)", &maxInFlight},
		{"max-db-size", "HOOKWITNESS_MAX_DB_SIZE", "", "a size limit for the database, with MiB/GiB (at least 64MiB): over it the oldest bodies, then requests, are dropped at the minute tick, normal ones before anomalies, pinned never (default: none)", &maxDBSize},
		{"retention", "HOOKWITNESS_RETENTION", DefaultRetention, "normal requests kept per endpoint (and in Unmatched) by default; the oldest are pruned; each endpoint can set its own", &retention},
		{"retention-anomalies", "HOOKWITNESS_RETENTION_ANOMALIES", DefaultAnomalies, "anomalies (not answered 2xx, a failed verdict, a redelivery) kept per endpoint by default, apart from the normal requests; 0: no separate lane", &anomalies},
		{"admin-addr", "HOOKWITNESS_ADMIN_ADDR", "", "listen address (host:port) of the web UI, REST API and MCP; empty: they live under -admin-prefix on -addr", &c.AdminAddr},
		{"admin-prefix", "HOOKWITNESS_ADMIN_PREFIX", DefaultAdminPrefix, "path prefix of the web UI; requests under it are never caught; / (the root) only with -admin-addr", &c.AdminPrefix},
		{"public-url", "HOOKWITNESS_PUBLIC_URL", "", "base URL senders reach the catch listener at, shown in the UI and the API (for proxies and tunnels), e.g. https://hooks.example.com", &c.PublicURL},
		{"admin-hosts", "HOOKWITNESS_ADMIN_HOSTS", "", "comma-separated host names the web UI answers to, besides IP addresses and localhost", &adminHosts},
		{"max-wait", "HOOKWITNESS_MAX_WAIT", DefaultMaxWait, "longest reply delay or timeout hold an endpoint may use, e.g. 30s or 5m", &maxWait},
		{"import", "HOOKWITNESS_IMPORT", "", "a setup file (exported from hookwitness) applied at every start: its endpoints are created or their behaviour replaced; secrets keep the values they have", &c.Import},
		{"trusted-proxies", "HOOKWITNESS_TRUSTED_PROXIES", "", "comma-separated addresses or CIDRs of reverse proxies whose X-Forwarded-For names the client (for the login limit); list only your own proxy", &proxies},
		{"mcp-max-sessions", "HOOKWITNESS_MCP_MAX_SESSIONS", DefaultMCPSessions, "most MCP sessions open at once; a client over it is answered 503 at initialize; 0: no limit", &mcpSessions},
		{"replay-allow", "HOOKWITNESS_REPLAY_ALLOW", "", "where caught requests may be replayed to: comma-separated host names, host:port, addresses or CIDRs, or none to turn replay off (default: anywhere but link-local and cloud-metadata addresses)", &replayAllow},
		{"admin-url", "HOOKWITNESS_ADMIN_URL", "", "the admin's external address, e.g. https://hooks-admin.example.com: the base of the OpenID Connect redirect URI (default: -public-url when the admin shares its listener)", &c.AdminURL},
		{"oidc-providers", "HOOKWITNESS_OIDC_PROVIDERS", "", "comma-separated names of OpenID Connect providers to sign in with, e.g. google,entra; each set by HOOKWITNESS_OIDC_<NAME>_ISSUER, _CLIENT_ID, _CLIENT_SECRET_FILE, _LABEL, _ALLOWED_DOMAINS", &oidcProviders},
	}

	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	// The flag package would print the whole usage after every error; keep
	// errors to one line and print the usage only when it is asked for.
	fs.SetOutput(io.Discard)
	for _, s := range settings {
		def := s.def
		if v := getenv(s.env); v != "" {
			def = v
		}
		fs.StringVar(s.dst, s.flag, def, s.usage+" (env "+s.env+")")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(out)
			fmt.Fprintln(out, "Usage: hookwitness serve [flags]")
			fs.PrintDefaults()
		}
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	var err error
	if c.MaxBody, err = parseSize(maxBody); err != nil {
		return Config{}, fmt.Errorf("invalid max body %q: %w", maxBody, err)
	}
	if c.MaxBody > MaxBodyLimit {
		return Config{}, fmt.Errorf("invalid max body %q: want at most 512 MiB: larger bodies cannot be stored in sqlite", maxBody)
	}
	c.MaxInFlight = max(minInFlight, 2*c.MaxBody)
	if maxInFlight != "" {
		if c.MaxInFlight, err = parseSize(maxInFlight); err != nil {
			return Config{}, fmt.Errorf("invalid max in-flight %q: %w", maxInFlight, err)
		}
		if c.MaxInFlight < c.MaxBody {
			return Config{}, fmt.Errorf("invalid max in-flight %q: want at least max body (%d bytes), or no body that size could be read", maxInFlight, c.MaxBody)
		}
	}
	if maxDBSize != "" {
		if c.MaxDBSize, err = parseSize(maxDBSize); err != nil {
			return Config{}, fmt.Errorf("invalid max db size %q: %w", maxDBSize, err)
		}
		if c.MaxDBSize < MinDBSize {
			return Config{}, fmt.Errorf("invalid max db size %q: want at least 64 MiB", maxDBSize)
		}
	}
	n, err := strconv.Atoi(retention)
	if err != nil || n < 1 {
		return Config{}, fmt.Errorf("invalid retention %q: want a whole number of at least 1", retention)
	}
	c.Retention = n
	if c.RetentionAnomalies, err = strconv.Atoi(anomalies); err != nil || c.RetentionAnomalies < 0 || c.RetentionAnomalies > 1_000_000 {
		return Config{}, fmt.Errorf("invalid retention anomalies %q: want a whole number from 0 to 1000000", anomalies)
	}
	if n > 1_000_000 {
		return Config{}, fmt.Errorf("invalid retention %q: want at most 1000000", retention)
	}
	if c.MCPMaxSessions, err = strconv.Atoi(mcpSessions); err != nil || c.MCPMaxSessions < 0 {
		return Config{}, fmt.Errorf("invalid MCP max sessions %q: want a whole number, 0 for no limit", mcpSessions)
	}
	if c.MaxWait, err = time.ParseDuration(maxWait); err != nil || c.MaxWait < time.Second || c.MaxWait > time.Hour {
		return Config{}, fmt.Errorf("invalid max wait %q: want a duration from 1s to 1h, e.g. 30s or 5m", maxWait)
	}
	for name := range strings.SplitSeq(adminHosts, ",") {
		if name = strings.TrimSpace(name); name != "" {
			c.AdminHosts = append(c.AdminHosts, name)
		}
	}
	if c.TrustedProxies, err = access.ParseProxies(proxies); err != nil {
		return Config{}, fmt.Errorf("invalid trusted proxies: %w", err)
	}
	if c.ReplayAllow, err = send.ParsePolicy(replayAllow); err != nil {
		return Config{}, fmt.Errorf("invalid replay allow list: %w", err)
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	c.PublicURL = strings.TrimSuffix(c.PublicURL, "/")
	c.AdminURL = strings.TrimSuffix(c.AdminURL, "/")
	if c.AdminPrefix == "/" {
		c.AdminPrefix = "" // the root of the admin's own port
	}
	if c.OIDC, err = parseProviders(oidcProviders, getenv); err != nil {
		return Config{}, err
	}
	if len(c.OIDC) > 0 && c.AdminOrigin() == "" {
		return Config{}, errors.New("OIDC needs the admin's address: set -admin-url / HOOKWITNESS_ADMIN_URL")
	}
	return c, nil
}

var (
	providerName = regexp.MustCompile(`^[a-z0-9]{1,32}$`)
	dnsName      = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// parseProviders reads the providers named in list from the environment;
// every error names the variable.
func parseProviders(list string, getenv func(string) string) ([]Provider, error) {
	var out []Provider
	seen := map[string]bool{}
	for name := range strings.SplitSeq(list, ",") {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		if !providerName.MatchString(name) {
			return nil, fmt.Errorf("HOOKWITNESS_OIDC_PROVIDERS: invalid name %q: want 1-32 of a-z and 0-9", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("HOOKWITNESS_OIDC_PROVIDERS: %s is listed twice", name)
		}
		seen[name] = true
		p := Provider{Name: name, Env: "HOOKWITNESS_OIDC_" + strings.ToUpper(name) + "_"}
		p.Issuer = getenv(p.Env + "ISSUER")
		p.ClientID = getenv(p.Env + "CLIENT_ID")
		p.SecretFile = getenv(p.Env + "CLIENT_SECRET_FILE")
		p.Label = getenv(p.Env + "LABEL")
		if err := checkIssuer(p.Issuer); err != nil {
			return nil, fmt.Errorf("%sISSUER: %w", p.Env, err)
		}
		if p.ClientID == "" || strings.TrimSpace(p.ClientID) != p.ClientID {
			return nil, fmt.Errorf("%sCLIENT_ID: want the client id the provider gave", p.Env)
		}
		if p.SecretFile == "" {
			return nil, fmt.Errorf("%sCLIENT_SECRET_FILE: want a file holding the client secret", p.Env)
		}
		if p.Label == "" {
			p.Label = strings.ToUpper(name[:1]) + name[1:]
		}
		if n := utf8.RuneCountInString(p.Label); n > 32 || strings.IndexFunc(p.Label, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
			return nil, fmt.Errorf("%sLABEL: want 1-32 printable characters", p.Env)
		}
		for d := range strings.SplitSeq(getenv(p.Env+"ALLOWED_DOMAINS"), ",") {
			if d = strings.ToLower(strings.TrimSpace(d)); d == "" {
				continue
			}
			if !dnsName.MatchString(d) {
				return nil, fmt.Errorf("%sALLOWED_DOMAINS: invalid domain %q: want a name such as example.com (no @, no wildcards; subdomains are not implied)", p.Env, d)
			}
			p.AllowedDomains = append(p.AllowedDomains, d)
		}
		out = append(out, p)
	}
	return out, nil
}

// checkIssuer: https, or http on a loopback host (a local provider, tests);
// no query or fragment.
func checkIssuer(s string) error {
	if s == "" {
		return errors.New("want the issuer URL, as the provider's discovery document states it")
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || !strings.Contains(s, "://") {
		return errors.New("want an absolute URL such as https://accounts.google.com")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.User != nil {
		return errors.New("want no query, fragment or user part")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if h := u.Hostname(); h == "localhost" || net.ParseIP(h) != nil && net.ParseIP(h).IsLoopback() {
			return nil
		}
		return errors.New("want an https URL (http only on a loopback host)")
	}
	return errors.New("want an https URL")
}

var units = map[string]int64{"": 1, "B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30}

// parseSize reads a byte count such as 1048576, 512KiB or 10MiB.
func parseSize(s string) (int64, error) {
	i := strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' })
	if i < 0 {
		i = len(s)
	}
	num, unit := s[:i], strings.TrimSpace(s[i:])
	mul, ok := units[unit]
	n, err := strconv.ParseInt(num, 10, 64)
	if num == "" || err != nil || !ok {
		return 0, errors.New("want bytes, or a whole number with KiB, MiB or GiB")
	}
	if n < 1 || n > (1<<40)/mul {
		return 0, errors.New("want at least 1 byte and at most 1024 GiB")
	}
	return n * mul, nil
}

// segment is one path segment of the admin prefix (and of endpoint paths).
var segment = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

func (c Config) validate() error {
	if err := checkAddr("listen address", c.Addr); err != nil {
		return err
	}
	if c.AdminAddr != "" {
		if err := checkAddr("admin address", c.AdminAddr); err != nil {
			return err
		}
		if sameListen(c.Addr, c.AdminAddr) {
			return fmt.Errorf("invalid admin address %q: it is the same address as the listen address %q; leave it empty to keep the admin under the prefix there", c.AdminAddr, c.Addr)
		}
	}
	if err := checkPublicURL(c.PublicURL); err != nil {
		return fmt.Errorf("invalid public URL %q: %w", c.PublicURL, err)
	}
	if err := checkPublicURL(c.AdminURL); err != nil {
		return fmt.Errorf("invalid admin URL %q: %w", c.AdminURL, err)
	}
	if u, err := url.Parse(c.AdminURL); c.AdminURL != "" && err == nil && u.Path != "" && u.Path != "/" {
		return fmt.Errorf("invalid admin URL %q: want an origin with no path, e.g. https://hooks-admin.example.com (the admin prefix is added to it)", c.AdminURL)
	}
	if c.DBPath == "" {
		return errors.New("database path must not be empty")
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("invalid log format %q: want text or json", c.LogFormat)
	}
	if c.AdminPrefix == "/" {
		if c.AdminAddr == "" {
			return errors.New(`invalid admin prefix "/": the admin can be at the root only on its own address (-admin-addr)`)
		}
	} else if err := checkPrefix(c.AdminPrefix); err != nil {
		return fmt.Errorf("invalid admin prefix %q: %w", c.AdminPrefix, err)
	}
	return nil
}

func checkAddr(name, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid %s %q: want host:port", name, addr)
	}
	if host != "" && net.ParseIP(host) == nil && host != "localhost" {
		return fmt.Errorf("invalid %s %q: host must be an IP address or localhost", name, addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("invalid %s %q: port must be 0-65535", name, addr)
	}
	return nil
}

// sameListen reports whether two valid listen addresses would take the same
// port: equal ports (other than 0, which picks a free one) on hosts that
// overlap (equal, either one all interfaces, or localhost and a loopback
// address).
func sameListen(a, b string) bool {
	ah, ap, _ := net.SplitHostPort(a)
	bh, bp, _ := net.SplitHostPort(b)
	if ap != bp || ap == "0" {
		return false
	}
	all := func(h string) bool { ip := net.ParseIP(h); return h == "" || ip != nil && ip.IsUnspecified() }
	loop := func(h string) bool { ip := net.ParseIP(h); return h == "localhost" || ip != nil && ip.IsLoopback() }
	return ah == bh || all(ah) || all(bh) || (ah == "localhost" || bh == "localhost") && loop(ah) && loop(bh)
}

func checkPublicURL(s string) error {
	if s == "" {
		return nil
	}
	u, err := url.Parse(s)
	switch {
	case err != nil || !strings.Contains(s, "://"):
		return errors.New("want an absolute URL such as https://hooks.example.com")
	case u.Scheme != "http" && u.Scheme != "https":
		return errors.New("want an http or https URL")
	case u.User != nil:
		return errors.New("want no user or password in it")
	case u.Host == "":
		return errors.New("want a host")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(s, "?#"):
		return errors.New("want no query or fragment: endpoint paths are appended to it")
	}
	return nil
}

func checkPrefix(p string) error {
	if !strings.HasPrefix(p, "/") || p == "/" || strings.HasSuffix(p, "/") {
		return errors.New(`want a path such as /admin: a leading "/", no trailing "/"`)
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if !segment.MatchString(seg) || seg == "." || seg == ".." {
			return errors.New("each segment may use only letters, digits and . _ ~ -")
		}
	}
	if p == "/healthz" {
		return errors.New("/healthz is taken by the health check")
	}
	return nil
}
