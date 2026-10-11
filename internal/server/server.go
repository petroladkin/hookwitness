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
	"time"

	"github.com/petroladkin/hookwitness/internal/version"
)

// ShutdownTimeout bounds how long in-flight requests may run after a stop
// signal before their connections are closed.
const ShutdownTimeout = 10 * time.Second

// Pinger reports whether the storage behind the server answers.
type Pinger interface {
	Ping(ctx context.Context) error
}

// New returns the server's root handler.
func New(db Pinger, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz(db, log))
	return mux
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
func Serve(ctx context.Context, ln net.Listener, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

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
