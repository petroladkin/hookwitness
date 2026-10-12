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

	"github.com/petroladkin/hookwitness/internal/catch"
	"github.com/petroladkin/hookwitness/internal/config"
	"github.com/petroladkin/hookwitness/internal/live"
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
	if same, err := st.KeyCheck(ctx, keys.Check()); err == nil && !same {
		log.Warn("this secret key is not the one this database was used with: sealed signing keys cannot be opened and stored tokens never match; "+
			"start with the original key (HOOKWITNESS_SECRET_KEY or the .key file next to the database), or replace the secrets", "secret_key", keySource)
	}
	warnUnreadableSecrets(ctx, st, keys, log)

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		log.Error("cannot listen", "addr", cfg.Addr, "err", err)
		return 1
	}
	schema, _ := st.SchemaVersion(ctx)
	log.Info("hookwitness started",
		"version", version.Version, "commit", version.Commit,
		"addr", ln.Addr().String(), "db", cfg.DBPath, "schema", schema,
		"ui", "http://"+ln.Addr().String()+cfg.AdminPrefix+"/",
		"max_body", cfg.MaxBody, "retention", cfg.Retention, "secret_key", keySource)

	broker := live.New()
	catcher := &catch.Handler{Store: st, Live: broker, Log: log, MaxBody: cfg.MaxBody, Retention: cfg.Retention, MaxWait: cfg.MaxWait, Keys: keys}
	ui := web.New(st, broker, log, web.Options{
		Prefix: cfg.AdminPrefix, MaxBody: cfg.MaxBody, Retention: cfg.Retention, Hosts: cfg.AdminHosts, MaxWait: cfg.MaxWait, Keys: keys,
	})
	root := server.New(server.Options{
		DB:          st,
		Log:         log,
		AdminPrefix: cfg.AdminPrefix,
		Admin:       ui,
		Catch:       catcher,
	})
	err = server.Serve(ctx, ln, root, log, ui.StopStreams, catcher.Stop)
	catcher.Wait() // timeouts and drops record their outcome before the store closes
	if err != nil {
		log.Error("server stopped with an error", "err", err)
		return 1
	}
	log.Info("stopped")
	return 0
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
