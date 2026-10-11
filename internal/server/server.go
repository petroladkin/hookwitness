// Package server is the HTTP side of hookwitness: the routes and the
// listener's lifecycle.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/catch"
	"github.com/petroladkin/hookwitness/internal/version"
	"github.com/petroladkin/hookwitness/internal/wire"
)

// ShutdownTimeout bounds how long in-flight requests may run after a stop
// signal before their connections are closed.
const ShutdownTimeout = 10 * time.Second

// Pinger reports whether the storage behind the server answers.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Catcher records requests to paths that are not reserved.
type Catcher interface {
	Catch(w http.ResponseWriter, r *http.Request, headers []wire.Header, ok bool)
}

// Options are the parts the root handler routes to.
type Options struct {
	DB          Pinger
	Log         *slog.Logger
	AdminPrefix string       // e.g. /admin; the web UI owns it and every path under it
	Admin       http.Handler // the web UI, given the full path
	Catch       Catcher      // everything else
}

// New returns the server's root handler. It routes on the raw request path:
// /healthz and the admin prefix are reserved, every other path is caught
// as the sender wrote it (no cleaning, no redirects).
func New(o Options) http.Handler {
	health := http.NewServeMux()
	health.HandleFunc("GET /healthz", healthz(o.DB, o.Log))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every request takes its recorded head, whichever route serves it,
		// so the next request on the connection gets its own.
		headers, ok := wire.Take(r)
		p := catch.RawPath(r)
		switch {
		case p == "/healthz":
			health.ServeHTTP(w, r)
		case o.Admin != nil && (p == o.AdminPrefix || strings.HasPrefix(p, o.AdminPrefix+"/")):
			o.Admin.ServeHTTP(w, r)
		case o.Catch != nil:
			o.Catch.Catch(w, r, headers, ok)
		default:
			http.NotFound(w, r)
		}
	})
}

func healthz(db Pinger, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		status, code := "ok", http.StatusOK
		if err := db.Ping(r.Context()); err != nil {
			log.Error("health check: database does not answer", "err", err)
			status, code = "database unavailable", http.StatusServiceUnavailable
		}
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(struct {
			Status  string `json:"status"`
			Version string `json:"version"`
		}{status, version.Version})
	}
}

// Serve accepts connections on ln until ctx is cancelled, then stops taking
// new connections and waits up to ShutdownTimeout for in-flight requests.
// onShutdown functions run when the shutdown starts; long-lived responses
// (live feeds) use them to end, which Shutdown would otherwise wait for.
func Serve(ctx context.Context, ln net.Listener, h http.Handler, log *slog.Logger, onShutdown ...func()) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout or WriteTimeout: live feeds stay open for hours.
		// The catcher bounds body reads itself.
		IdleTimeout: 2 * time.Minute,
		ErrorLog:    slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		ConnContext: wire.ConnContext,
	}
	for _, f := range onShutdown {
		srv.RegisterOnShutdown(f)
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(wire.Listener(ln)) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down", "timeout", ShutdownTimeout.String())
	sctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		srv.Close()
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
