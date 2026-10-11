// Command hookwitness catches webhooks, answers them the way you script, and
// keeps the evidence.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

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
		Prefix: cfg.AdminPrefix, MaxBody: cfg.MaxBody, Retention: cfg.Retention, Hosts: cfg.AdminHosts, MaxWait: cfg.MaxWait,
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

func newLogger(format string, w io.Writer) *slog.Logger {
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, nil))
	}
	return slog.New(slog.NewTextHandler(w, nil))
}
