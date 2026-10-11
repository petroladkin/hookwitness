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
	"regexp"
	"strconv"
	"strings"
)

// Config is the resolved configuration of the server.
type Config struct {
	Addr        string // listen address, host:port
	DBPath      string // sqlite file
	LogFormat   string // "text" or "json"
	MaxBody     int64  // largest request body kept, in bytes
	Retention   int    // requests kept per endpoint
	AdminPrefix string // path prefix of the web UI, e.g. /admin
}

// Defaults.
const (
	DefaultAddr        = "127.0.0.1:8080"
	DefaultDBPath      = "hookwitness.db"
	DefaultLogFormat   = "text"
	DefaultMaxBody     = "10MiB"
	DefaultRetention   = "1000"
	DefaultAdminPrefix = "/admin"
)

type setting struct {
	flag, env, def, usage string
	dst                   *string
}

// Parse reads args (without the subcommand name) and the environment through
// getenv. Usage and errors from the flag package go to out.
func Parse(args []string, getenv func(string) string, out io.Writer) (Config, error) {
	var c Config
	var maxBody, retention string
	settings := []setting{
		{"addr", "HOOKWITNESS_ADDR", DefaultAddr, "listen address (host:port)", &c.Addr},
		{"db", "HOOKWITNESS_DB", DefaultDBPath, "path to the sqlite database file", &c.DBPath},
		{"log-format", "HOOKWITNESS_LOG_FORMAT", DefaultLogFormat, "log format: text or json", &c.LogFormat},
		{"max-body", "HOOKWITNESS_MAX_BODY", DefaultMaxBody, "largest request body to keep, in bytes or with KiB/MiB/GiB; a larger one is answered 413", &maxBody},
		{"retention", "HOOKWITNESS_RETENTION", DefaultRetention, "requests kept per endpoint; the oldest are pruned", &retention},
		{"admin-prefix", "HOOKWITNESS_ADMIN_PREFIX", DefaultAdminPrefix, "path prefix of the web UI; requests under it are never caught", &c.AdminPrefix},
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
	n, err := strconv.Atoi(retention)
	if err != nil || n < 1 {
		return Config{}, fmt.Errorf("invalid retention %q: want a whole number of at least 1", retention)
	}
	c.Retention = n
	return c, c.validate()
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
	host, port, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: want host:port", c.Addr)
	}
	if host != "" && net.ParseIP(host) == nil && host != "localhost" {
		return fmt.Errorf("invalid listen address %q: host must be an IP address or localhost", c.Addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("invalid listen address %q: port must be 0-65535", c.Addr)
	}
	if c.DBPath == "" {
		return errors.New("database path must not be empty")
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("invalid log format %q: want text or json", c.LogFormat)
	}
	if err := checkPrefix(c.AdminPrefix); err != nil {
		return fmt.Errorf("invalid admin prefix %q: %w", c.AdminPrefix, err)
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
