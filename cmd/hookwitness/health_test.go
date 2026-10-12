package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbeAddr(t *testing.T) {
	for in, want := range map[string]string{
		":8080":          "127.0.0.1:8080",
		"0.0.0.0:9000":   "127.0.0.1:9000",
		"[::]:9000":      "127.0.0.1:9000",
		"127.0.0.1:9000": "127.0.0.1:9000",
		"[::1]:9000":     "[::1]:9000",
		"localhost:9000": "localhost:9000",
	} {
		if got := probeAddr(in); got != want {
			t.Errorf("probeAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func healthOf(t *testing.T, status int) (int, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	var out bytes.Buffer
	code := run(context.Background(), []string{"health", "-addr", srv.Listener.Addr().String()}, noenv, &out, &out)
	return code, out.String()
}

func TestHealthOK(t *testing.T) {
	if code, out := healthOf(t, http.StatusOK); code != 0 || out != "" {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

func TestHealthUnhealthy(t *testing.T) {
	code, out := healthOf(t, http.StatusServiceUnavailable)
	if code != 1 || !strings.Contains(out, "503") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

func TestHealthNothingListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	var out bytes.Buffer
	code := run(context.Background(), []string{"health", "-addr", addr}, noenv, &out, &out)
	if code != 1 || !strings.Contains(out.String(), "unhealthy") {
		t.Fatalf("exit %d, output %q", code, out.String())
	}
}

func TestHealthReadsAddrFromEnv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	env := func(k string) string {
		if k == "HOOKWITNESS_ADDR" {
			return srv.Listener.Addr().String()
		}
		return ""
	}
	var out bytes.Buffer
	if code := run(context.Background(), []string{"health"}, env, &out, &out); code != 0 {
		t.Fatalf("exit %d, output %q", code, out.String())
	}
}
