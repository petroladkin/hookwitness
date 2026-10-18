// Command hookwitness catches webhooks, answers them the way you script, and
// keeps the evidence.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/petroladkin/hookwitness/internal/api"
	"github.com/petroladkin/hookwitness/internal/catch"
	"github.com/petroladkin/hookwitness/internal/config"
	"github.com/petroladkin/hookwitness/internal/live"
	"github.com/petroladkin/hookwitness/internal/mcp"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/server"
	"github.com/petroladkin/hookwitness/internal/store"
	"github.com/petroladkin/hookwitness/internal/version"
	"github.com/petroladkin/hookwitness/internal/web"
)

const usage = `Usage: hookwitness <command> [flags]

Commands:
  serve     start the server (hookwitness serve -h for its flags)
  health    exit 0 when the server at -addr / HOOKWITNESS_ADDR answers /healthz
  mcp       bridge an MCP client on stdio to a server's /mcp (hookwitness mcp -url http://127.0.0.1:8080)
  version   print the version and exit
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// After the first signal, restore the default handling so that a second
	// Ctrl-C during the graceful shutdown kills the process at once.
	go func() { <-ctx.Done(); stop() }()
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is main without the process: it returns the exit code.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:], getenv, stderr)
	case "health":
		return health(ctx, args[1:], getenv, stderr)
	case "mcp":
		return mcpBridge(ctx, args[1:], stderr)
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, "hookwitness", version.String())
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "hookwitness: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func serve(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) int {
	cfg, err := config.Parse(args, getenv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "hookwitness:", err)
		return 2
	}
	log := newLogger(cfg.LogFormat, stderr)

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		if ctx.Err() != nil {
			log.Info("stopped before the server started")
			return 0
		}
		log.Error("cannot open the database", "db", cfg.DBPath, "err", err)
		return 1
	}
	defer st.Close()

	// Endpoint secrets are sealed or hashed with this key; a database copied
	// without it reveals none of them.
	keys, err := secret.Load(getenv("HOOKWITNESS_SECRET_KEY"), cfg.DBPath+".key")
	if err != nil {
		log.Error("cannot load the secret key", "err", err)
		return 1
	}
	keySource := "HOOKWITNESS_SECRET_KEY"
	if getenv("HOOKWITNESS_SECRET_KEY") == "" {
		keySource = cfg.DBPath + ".key"
	}
	for _, n := range keys.Notes() {
		log.Warn(n, "secret_key", keySource)
	}
	checkKey(ctx, st, keys, keySource, log)
	warnUnreadableSecrets(ctx, st, keys, log)

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		log.Error("cannot listen", "addr", cfg.Addr, "err", err)
		return 1
	}
	separate := cfg.AdminAddr != ""
	adminLn := ln
	if separate {
		if adminLn, err = net.Listen("tcp", cfg.AdminAddr); err != nil {
			ln.Close()
			log.Error("cannot listen for the admin", "admin_addr", cfg.AdminAddr, "err", err)
			return 1
		}
	} else {
		warnShadowedEndpoints(ctx, st, cfg.AdminPrefix, log)
	}
	schema, _ := st.SchemaVersion(ctx)
	attrs := []any{"version", version.Version, "commit", version.Commit, "addr", ln.Addr().String()}
	if separate {
		attrs = append(attrs, "admin_addr", adminLn.Addr().String())
	}
	if cfg.PublicURL != "" {
		attrs = append(attrs, "public_url", cfg.PublicURL)
	}
	attrs = append(attrs, "db", cfg.DBPath, "schema", schema,
		"ui", "http://"+adminLn.Addr().String()+cfg.AdminPrefix+"/",
		"api", "http://"+adminLn.Addr().String()+api.Base(cfg.AdminPrefix),
		"max_body", cfg.MaxBody, "max_inflight", cfg.MaxInFlight, "retention", cfg.Retention, "secret_key", keySource)
	log.Info("hookwitness started", attrs...)

	broker := live.New()
	catcher := &catch.Handler{Store: st, Live: broker, Log: log, MaxBody: cfg.MaxBody, MaxInFlight: cfg.MaxInFlight,
		Retention: cfg.Retention, MaxWait: cfg.MaxWait, Keys: keys}
	ui := web.New(st, broker, log, web.Options{
		Prefix: cfg.AdminPrefix, Separate: separate, PublicURL: cfg.PublicURL, CatchAddr: ln.Addr().String(),
		MaxBody: cfg.MaxBody, Retention: cfg.Retention, Hosts: cfg.AdminHosts, MaxWait: cfg.MaxWait, Keys: keys,
	})
	o := server.Options{DB: st, Log: log, AdminPrefix: cfg.AdminPrefix, Admin: ui, Catch: catcher}
	if !separate {
		err = server.Serve(ctx, ln, server.New(o), log, ui.StopStreams, catcher.Stop)
	} else {
		err = serveBoth(ctx, log,
			func(ctx context.Context) error { return server.Serve(ctx, ln, server.CatchRoot(o), log, catcher.Stop) },
			func(ctx context.Context) error {
				return server.Serve(ctx, adminLn, server.AdminRoot(o), log, ui.StopStreams)
			})
	}
	catcher.Wait() // timeouts and drops record their outcome before the store closes
	if err != nil {
		log.Error("server stopped with an error", "err", err)
		return 1
	}
	log.Info("stopped")
	return 0
}

// serveBoth runs the catch and the admin listener together: a stop signal
// stops both, and either one failing stops the other. It returns the first
// error.
func serveBoth(ctx context.Context, log *slog.Logger, servers ...func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			err := s(ctx)
			if err != nil {
				cancel()
			}
			errc <- err
		}()
	}
	var first error
	for range servers {
		if err := <-errc; err != nil && first == nil {
			first = err
		}
	}
	return first
}

// warnShadowedEndpoints names the endpoints whose paths the admin prefix
// covers: made while the admin had its own address, they are not caught
// while the admin shares the listener (their data stays).
func warnShadowedEndpoints(ctx context.Context, st *store.Store, prefix string, log *slog.Logger) {
	eps, err := st.Endpoints(ctx)
	if err != nil {
		return
	}
	var shadowed []string
	for _, e := range eps {
		if e.Path == prefix || strings.HasPrefix(e.Path, prefix+"/") {
			shadowed = append(shadowed, e.Path)
		}
	}
	if len(shadowed) > 0 {
		log.Warn("endpoints under the admin prefix are shadowed by the admin and catch nothing; give the admin its own address (-admin-addr) or change their paths",
			"admin_prefix", prefix, "paths", strings.Join(shadowed, ","))
	}
}

// health asks a running server's /healthz and exits 0 when it answers 200:
// the probe of a container's HEALTHCHECK, since the image has no curl. It
// reads the same flags and environment as serve, so -addr and
// HOOKWITNESS_ADDR mean the same thing to both.
func health(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) int {
	cfg, err := config.Parse(args, getenv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "hookwitness:", err)
		return 2
	}
	url := "http://" + probeAddr(cfg.Addr) + "/healthz"
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintln(stderr, "hookwitness: unhealthy:", err)
		return 1
	}
	// Never through a proxy from the environment: the server is local.
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(stderr, "hookwitness: unhealthy:", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "hookwitness: unhealthy: %s answered %d\n", url, resp.StatusCode)
		return 1
	}
	return 0
}

// probeAddr turns a listen address into one to connect to: a server
// listening on all interfaces (":8080", "0.0.0.0:8080", "[::]:8080") is
// reached on the IPv4 loopback.
func probeAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// warnUnreadableSecrets logs the endpoints whose signing keys this key cannot
// open (the database came without its key file, or the key changed): their
// checks will say "not verifiable" until the secrets are replaced.
// checkKey warns when the key is not the one the database was used with, or
// when that cannot be told.
func checkKey(ctx context.Context, st *store.Store, keys *secret.Key, keySource string, log *slog.Logger) {
	same, err := st.KeyCheck(ctx, keys.Check())
	switch {
	case err != nil:
		log.Warn("cannot check the secret key against the database", "secret_key", keySource, "err", err)
	case !same:
		log.Warn("this secret key is not the one this database was used with: sealed signing keys cannot be opened and stored tokens never match; "+
			"start with the original key (HOOKWITNESS_SECRET_KEY or the .key file next to the database), or replace the secrets", "secret_key", keySource)
	}
}

func warnUnreadableSecrets(ctx context.Context, st *store.Store, keys *secret.Key, log *slog.Logger) {
	eps, err := st.Endpoints(ctx)
	if err != nil {
		return
	}
	for _, e := range eps {
		cfg, err := emulate.Parse(e.Config)
		if err != nil {
			continue
		}
		var bad []string
		for _, s := range cfg.Secrets {
			if s.Kind != emulate.SecretKey {
				continue
			}
			if _, err := keys.Open(s.Sealed); err != nil {
				bad = append(bad, s.Name)
			}
		}
		if len(bad) > 0 {
			log.Warn("secrets cannot be decrypted with this secret key; replace them or use the key they were saved with",
				"path", e.Path, "secrets", strings.Join(bad, ","))
		}
	}
}

func newLogger(format string, w io.Writer) *slog.Logger {
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, nil))
	}
	return slog.New(slog.NewTextHandler(w, nil))
}

// stdin and stdout of the MCP bridge; tests replace them.
var (
	bridgeIn  io.ReadCloser  = os.Stdin
	bridgeOut io.WriteCloser = os.Stdout
)

// mcpBridge serves an MCP client that speaks only stdio: every message goes
// to the server's Streamable HTTP endpoint and back, unchanged.
func mcpBridge(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("url", "http://127.0.0.1:8080", "the hookwitness server (its /admin/mcp is used), or the full MCP URL")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	endpoint, err := mcp.Endpoint(*server)
	if err != nil {
		fmt.Fprintln(stderr, "hookwitness:", err)
		return 2
	}
	if err := mcp.Bridge(ctx, endpoint, http.DefaultClient, bridgeIn, bridgeOut); err != nil {
		fmt.Fprintln(stderr, "hookwitness mcp:", err)
		return 1
	}
	return 0
}
