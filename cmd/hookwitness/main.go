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

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/api"
	"github.com/petroladkin/hookwitness/internal/catch"
	"github.com/petroladkin/hookwitness/internal/config"
	"github.com/petroladkin/hookwitness/internal/live"
	"github.com/petroladkin/hookwitness/internal/mcp"
	"github.com/petroladkin/hookwitness/internal/replay"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/server"
	"github.com/petroladkin/hookwitness/internal/setup"
	"github.com/petroladkin/hookwitness/internal/store"
	"github.com/petroladkin/hookwitness/internal/version"
	"github.com/petroladkin/hookwitness/internal/web"
)

const usage = `Usage: hookwitness <command> [flags]

Commands:
  serve     start the server (hookwitness serve -h for its flags)
  health    exit 0 when the server at -addr / HOOKWITNESS_ADDR answers /healthz
  mcp       bridge an MCP client on stdio to a server's /mcp (hookwitness mcp -url http://127.0.0.1:8080)
  apply     create or replace the endpoints of a setup file on a running server (hookwitness apply -h)
  user      add, change, remove and list who may log in (hookwitness user -h)
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
	case "apply":
		return apply(ctx, args[1:], stdout, stderr)
	case "user":
		return userCmd(ctx, args[1:], getenv, stdout, stderr)
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

	reserved := cfg.AdminPrefix
	if cfg.AdminAddr != "" {
		reserved = ""
	}
	if cfg.Import != "" && !importSetup(ctx, st, cfg.Import, setup.Options{Reserved: reserved, MaxWait: cfg.MaxWait}, log) {
		return 1
	}

	machine, machineWarn := "", ""
	if f := getenv("HOOKWITNESS_API_TOKEN_FILE"); f != "" {
		if machine, machineWarn, err = access.ReadMachineToken(f); err != nil {
			log.Error("cannot read the machine token", "err", err)
			return 1
		}
	}
	auth := access.New(st, keys, machine)
	boot, err := bootstrap(ctx, st, auth, getenv)
	if err != nil {
		log.Error("cannot create the first admin", "err", err)
		return 1
	}
	users, err := st.Users(ctx)
	if err != nil {
		log.Error("cannot read the users", "err", err)
		return 1
	}

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
		"max_body", cfg.MaxBody, "max_inflight", cfg.MaxInFlight, "retention", cfg.Retention, "secret_key", keySource,
		"replay", cfg.ReplayAllow.String())
	if len(users) > 0 {
		attrs = append(attrs, "login", "on", "users", len(users))
	} else {
		attrs = append(attrs, "login", "off")
	}
	if machine != "" {
		attrs = append(attrs, "machine_token", "on")
	}
	log.Info("hookwitness started", attrs...)
	if boot != nil {
		log.Info(boot[0].(string), boot[1:]...)
	}
	if machineWarn != "" {
		log.Warn(machineWarn)
	}
	if len(users) == 0 && !loopback(adminLn.Addr()) {
		log.Warn("the admin is open without login on this address: if it is reachable, anyone can read every caught request and change every endpoint; "+
			"add a user to turn login on (HOOKWITNESS_ADMIN_USER, hookwitness user add, or the Users page)", "admin_addr", adminLn.Addr().String())
	}

	broker := live.New()
	catcher := &catch.Handler{Store: st, Live: broker, Log: log, MaxBody: cfg.MaxBody, MaxInFlight: cfg.MaxInFlight,
		Retention: cfg.Retention, MaxWait: cfg.MaxWait, Keys: keys}
	replays := replay.New(st, log, replay.Options{Policy: cfg.ReplayAllow, MaxWait: cfg.MaxWait, Budget: catcher})
	ui := web.New(st, broker, log, web.Options{
		Prefix: cfg.AdminPrefix, Separate: separate, PublicURL: cfg.PublicURL, CatchAddr: ln.Addr().String(),
		MaxBody: cfg.MaxBody, Retention: cfg.Retention, Hosts: cfg.AdminHosts, MaxWait: cfg.MaxWait, Keys: keys,
		Access: auth, Proxies: cfg.TrustedProxies, Replay: replays, MCPMaxSessions: cfg.MCPMaxSessions,
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
	replays.Wait() // so do replays
	if err != nil {
		log.Error("server stopped with an error", "err", err)
		return 1
	}
	log.Info("stopped")
	return 0
}

// loopback reports whether a listener only takes connections from this
// machine.
func loopback(a net.Addr) bool {
	ta, ok := a.(*net.TCPAddr)
	return ok && ta.IP.IsLoopback()
}

// bootstrap creates the first admin from HOOKWITNESS_ADMIN_USER and
// HOOKWITNESS_ADMIN_PASSWORD_FILE when the database has no users. It returns
// the log line to write (message, then attributes) or nil when neither is
// set; an incomplete or invalid setting is an error, which stops the start.
func bootstrap(ctx context.Context, st *store.Store, auth *access.Authenticator, getenv func(string) string) ([]any, error) {
	name, file := getenv("HOOKWITNESS_ADMIN_USER"), getenv("HOOKWITNESS_ADMIN_PASSWORD_FILE")
	switch {
	case name == "" && file == "":
		return nil, nil
	case name == "" || file == "":
		return nil, errors.New("set both HOOKWITNESS_ADMIN_USER and HOOKWITNESS_ADMIN_PASSWORD_FILE, or neither")
	}
	n, err := access.CheckName(name)
	if err != nil {
		return nil, fmt.Errorf("HOOKWITNESS_ADMIN_USER: %w", err)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("HOOKWITNESS_ADMIN_PASSWORD_FILE: %w", err)
	}
	pw := strings.TrimRight(string(b), "\r\n")
	if err := access.CheckPassword(pw); err != nil {
		return nil, fmt.Errorf("HOOKWITNESS_ADMIN_PASSWORD_FILE: %w", err)
	}
	if on, err := st.HasUsers(ctx); err != nil {
		return nil, err
	} else if on {
		return []any{"admin bootstrap skipped: the database already has users (the password file is read only then)", "user", n}, nil
	}
	if _, err := st.AddUser(ctx, n, store.RoleAdmin, auth.HashPassword(pw)); err != nil {
		return nil, err
	}
	return []any{"admin user created from HOOKWITNESS_ADMIN_USER: login is on", "user", n}, nil
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
			if s.Kind != emulate.SecretKey || !s.Set() {
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

// readTokenFile reads a -token-file: one line holding the token.
func readTokenFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	tok := strings.TrimSpace(string(b))
	if err != nil || tok == "" || strings.ContainsAny(tok, "\r\n") {
		return "", fmt.Errorf("-token-file %s: want one line holding the token", path)
	}
	return tok, nil
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
	server := fs.String("url", "http://127.0.0.1:8080", "the hookwitness server (its /admin/mcp is used), or the full MCP URL (e.g. http://127.0.0.1:8081/mcp for an admin at the root of its own address)")
	tokenFile := fs.String("token-file", "", "a file holding a token (from your account page) for a server with login on")
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
	client := http.DefaultClient
	if *tokenFile != "" {
		tok, err := readTokenFile(*tokenFile)
		if err != nil {
			fmt.Fprintln(stderr, "hookwitness:", err)
			return 2
		}
		client = mcp.WithToken(client, tok)
	}
	if err := mcp.Bridge(ctx, endpoint, client, bridgeIn, bridgeOut); err != nil {
		fmt.Fprintln(stderr, "hookwitness mcp:", err)
		return 1
	}
	return 0
}
