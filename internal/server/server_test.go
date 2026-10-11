package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/version"
)

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestHealthz(t *testing.T) {
	for _, tc := range []struct {
		name   string
		db     pinger
		code   int
		status string
	}{
		{"ok", pinger{}, 200, "ok"},
		{"db down", pinger{errors.New("closed")}, 503, "database unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			New(tc.db, quiet).ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
			if rec.Code != tc.code {
				t.Fatalf("status %d, want %d", rec.Code, tc.code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("content type %q", ct)
			}
			var body struct{ Status, Version string }
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Status != tc.status || body.Version != version.Version {
				t.Fatalf("body %+v", body)
			}
		})
	}
}

func TestHealthzRejectsOtherMethods(t *testing.T) {
	rec := httptest.NewRecorder()
	New(pinger{}, quiet).ServeHTTP(rec, httptest.NewRequest("POST", "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rec.Code)
	}
}

// An in-flight request finishes after the stop signal; new connections are
// refused; Serve returns nil.
func TestGracefulShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		io.WriteString(w, "done")
	})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, ln, h, quiet) }()

	addr := "http://" + ln.Addr().String()
	got := make(chan string, 1)
	go func() {
		resp, err := http.Get(addr + "/slow")
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		got <- string(b)
	}()
	<-started
	cancel()

	if body := <-got; body != "done" {
		t.Fatalf("in-flight request: %s", body)
	}
	if err := <-served; err != nil {
		t.Fatalf("Serve returned %v", err)
	}
	if _, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		t.Fatal("listener still accepts connections after shutdown")
	}
}
