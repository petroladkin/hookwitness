package config

import (
	"errors"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	c, err := Parse(nil, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Addr: DefaultAddr, DBPath: DefaultDBPath, LogFormat: DefaultLogFormat,
		MaxBody: 10 << 20, MaxInFlight: 256 << 20, Retention: 1000, AdminPrefix: "/admin", MaxWait: 5 * time.Minute}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v, want %+v", c, want)
	}
}

func TestEnvOverridesDefault(t *testing.T) {
	c, err := Parse(nil, env(map[string]string{
		"HOOKWITNESS_ADDR":         "127.0.0.1:9000",
		"HOOKWITNESS_DB":           "/tmp/x.db",
		"HOOKWITNESS_LOG_FORMAT":   "json",
		"HOOKWITNESS_MAX_BODY":     "512KiB",
		"HOOKWITNESS_RETENTION":    "50",
		"HOOKWITNESS_ADMIN_PREFIX": "/_hw/ui",
		"HOOKWITNESS_ADMIN_HOSTS":  "hooks.example.test, ui.example.test,",
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "127.0.0.1:9000" || c.DBPath != "/tmp/x.db" || c.LogFormat != "json" || len(c.AdminHosts) != 2 || c.AdminHosts[1] != "ui.example.test" ||
		c.MaxBody != 512<<10 || c.Retention != 50 || c.AdminPrefix != "/_hw/ui" {
		t.Fatalf("env not applied: %+v", c)
	}
}

func TestFlagWinsOverEnv(t *testing.T) {
	c, err := Parse([]string{"-addr", ":7000", "-log-format", "text", "-max-body", "2048", "-retention", "7", "-admin-prefix", "/ui", "-max-wait", "90s"},
		env(map[string]string{"HOOKWITNESS_ADDR": "127.0.0.1:9000", "HOOKWITNESS_LOG_FORMAT": "json",
			"HOOKWITNESS_MAX_BODY": "1MiB", "HOOKWITNESS_RETENTION": "9", "HOOKWITNESS_ADMIN_PREFIX": "/x"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":7000" || c.LogFormat != "text" || c.MaxBody != 2048 || c.Retention != 7 || c.AdminPrefix != "/ui" || c.MaxWait != 90*time.Second {
		t.Fatalf("flag did not win: %+v", c)
	}
}

func TestInvalid(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-addr", "8080"}, "want host:port"},
		{[]string{"-addr", "example.test:80"}, "IP address or localhost"},
		{[]string{"-addr", "127.0.0.1:99999"}, "port must be"},
		{[]string{"-db", ""}, "must not be empty"},
		{[]string{"-log-format", "xml"}, "want text or json"},
		{[]string{"-max-body", "10MB"}, "KiB, MiB or GiB"},
		{[]string{"-max-body", "0"}, "at least 1 byte"},
		{[]string{"-max-body", "-5"}, "KiB, MiB or GiB"},
		{[]string{"-max-body", "2000GiB"}, "at most 1024 GiB"},
		{[]string{"-max-body", "513MiB"}, "at most 512 MiB: larger bodies cannot be stored in sqlite"},
		{[]string{"-max-body", "1GiB"}, "at most 512 MiB"},
		{[]string{"-max-inflight", "1MiB"}, "at least max body"},
		{[]string{"-max-inflight", "lots"}, "KiB, MiB or GiB"},
		{[]string{"-retention", "0"}, "at least 1"},
		{[]string{"-max-wait", "soon"}, "1s to 1h"},
		{[]string{"-max-wait", "500ms"}, "1s to 1h"},
		{[]string{"-max-wait", "2h"}, "1s to 1h"},
		{[]string{"-retention", "many"}, "at least 1"},
		{[]string{"-admin-prefix", "admin"}, "leading"},
		{[]string{"-admin-prefix", "/"}, "leading"},
		{[]string{"-admin-prefix", "/admin/"}, "trailing"},
		{[]string{"-admin-prefix", "/a//b"}, "segment"},
		{[]string{"-admin-prefix", "/a b"}, "segment"},
		{[]string{"-admin-prefix", "/.."}, "segment"},
		{[]string{"-admin-prefix", "/healthz"}, "health check"},
		{[]string{"extra"}, "unexpected argument"},
		{[]string{"-nope"}, "not defined"},
	}
	for _, tc := range cases {
		_, err := Parse(tc.args, env(nil), io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Parse(%v) error = %v, want it to contain %q", tc.args, err, tc.want)
		}
	}
}

func TestHelpPrintsUsage(t *testing.T) {
	var out strings.Builder
	_, err := Parse([]string{"-h"}, env(nil), &out)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
	for _, want := range []string{"Usage: hookwitness serve", "-addr", "HOOKWITNESS_DB", "-log-format"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage lacks %q:\n%s", want, out.String())
		}
	}
}

func TestUnknownFlagPrintsNothing(t *testing.T) {
	var out strings.Builder
	if _, err := Parse([]string{"-bogus"}, env(nil), &out); err == nil {
		t.Fatal("want an error")
	}
	if out.Len() != 0 {
		t.Fatalf("printed %q; the caller prints the one-line error", out.String())
	}
}

// -max-body tops out at 512 MiB; -max-inflight defaults to the larger of
// 256 MiB and twice -max-body.
func TestBodyLimits(t *testing.T) {
	for _, c := range []struct {
		args     []string
		body, in int64
	}{
		{[]string{"-max-body", "512MiB"}, 512 << 20, 1 << 30},
		{[]string{"-max-body", "200MiB"}, 200 << 20, 400 << 20},
		{[]string{"-max-body", "1MiB"}, 1 << 20, 256 << 20},
		{[]string{"-max-body", "1MiB", "-max-inflight", "2MiB"}, 1 << 20, 2 << 20},
	} {
		got, err := Parse(c.args, env(nil), io.Discard)
		if err != nil || got.MaxBody != c.body || got.MaxInFlight != c.in {
			t.Errorf("%v: %d %d %v", c.args, got.MaxBody, got.MaxInFlight, err)
		}
	}
}
