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
	"strconv"
)

// Config is the resolved configuration of the server.
type Config struct {
	Addr      string // listen address, host:port
	DBPath    string // sqlite file
	LogFormat string // "text" or "json"
}

// Defaults.
const (
	DefaultAddr      = "127.0.0.1:8080"
	DefaultDBPath    = "hookwitness.db"
	DefaultLogFormat = "text"
)

type setting struct {
	flag, env, def, usage string
	dst                   *string
}

// Parse reads args (without the subcommand name) and the environment through
// getenv. Usage and errors from the flag package go to out.
func Parse(args []string, getenv func(string) string, out io.Writer) (Config, error) {
	var c Config
	settings := []setting{
		{"addr", "HOOKWITNESS_ADDR", DefaultAddr, "listen address (host:port)", &c.Addr},
		{"db", "HOOKWITNESS_DB", DefaultDBPath, "path to the sqlite database file", &c.DBPath},
		{"log-format", "HOOKWITNESS_LOG_FORMAT", DefaultLogFormat, "log format: text or json", &c.LogFormat},
	}

	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(out)
	for _, s := range settings {
		def := s.def
		if v := getenv(s.env); v != "" {
			def = v
		}
		fs.StringVar(s.dst, s.flag, def, s.usage+" (env "+s.env+")")
	}
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return c, c.validate()
}

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
	return nil
}
