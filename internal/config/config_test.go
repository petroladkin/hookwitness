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
		{[]string{"-trusted-proxies", "198.51.100.0/24,proxy.example.test"}, "invalid trusted proxies"},
		{[]string{"-max-wait", "2h"}, "1s to 1h"},
		{[]string{"-replay-allow", "none,localhost"}, "invalid replay allow list"},
		{[]string{"-replay-allow", "198.51.100.0/40"}, "invalid replay allow list"},
		{[]string{"-retention", "many"}, "at least 1"},
		{[]string{"-admin-prefix", "admin"}, "leading"},
		{[]string{"-admin-prefix", "/"}, "only on its own address (-admin-addr)"},
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

func TestAdminAddrAndPublicURL(t *testing.T) {
	c, err := Parse(nil, env(map[string]string{
		"HOOKWITNESS_ADMIN_ADDR": "127.0.0.1:8081",
		"HOOKWITNESS_PUBLIC_URL": "https://hooks.example.com/",
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminAddr != "127.0.0.1:8081" || c.PublicURL != "https://hooks.example.com" {
		t.Fatalf("env not applied: %+v", c)
	}
	c, err = Parse([]string{"-admin-addr", ":9001", "-public-url", "http://127.0.0.1:9000/hooks/"},
		env(map[string]string{"HOOKWITNESS_ADMIN_ADDR": "127.0.0.1:8081"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminAddr != ":9001" || c.PublicURL != "http://127.0.0.1:9000/hooks" {
		t.Fatalf("flags not applied: %+v", c)
	}
}

// With its own address the admin may live at the root of its port: the
// resolved prefix is then "".
func TestAdminAtTheRoot(t *testing.T) {
	c, err := Parse([]string{"-admin-addr", "127.0.0.1:8081", "-admin-prefix", "/"}, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminPrefix != "" {
		t.Fatalf("prefix = %q, want \"\"", c.AdminPrefix)
	}
	c, err = Parse([]string{"-admin-addr", "127.0.0.1:8081"}, env(nil), io.Discard)
	if err != nil || c.AdminPrefix != "/admin" {
		t.Fatalf("default prefix with an admin address: %q %v", c.AdminPrefix, err)
	}
}

func TestAdminAddrInvalid(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-admin-addr", "8081"}, "invalid admin address \"8081\": want host:port"},
		{[]string{"-admin-addr", "example.test:8081"}, "IP address or localhost"},
		{[]string{"-admin-addr", "127.0.0.1:70000"}, "port must be"},
		{[]string{"-addr", "127.0.0.1:8080", "-admin-addr", "127.0.0.1:8080"}, "the same address"},
		{[]string{"-addr", ":8080", "-admin-addr", "127.0.0.1:8080"}, "the same address"},
		{[]string{"-addr", "0.0.0.0:9000", "-admin-addr", "[::1]:9000"}, "the same address"},
		{[]string{"-addr", "localhost:9000", "-admin-addr", "127.0.0.1:9000"}, "the same address"},
		{[]string{"-public-url", "hooks.example.com"}, "invalid public URL"},
		{[]string{"-public-url", "ftp://x"}, "http or https"},
		{[]string{"-public-url", "https://x/?a=1"}, "no query"},
		{[]string{"-public-url", "https://x/#f"}, "no query"},
		{[]string{"-public-url", "https://user@x"}, "no user"},
		{[]string{"-public-url", "https://"}, "a host"},
	} {
		_, err := Parse(tc.args, env(nil), io.Discard)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Parse(%v) error = %v, want it to contain %q", tc.args, err, tc.want)
		}
	}
	for _, args := range [][]string{
		{"-addr", "127.0.0.1:0", "-admin-addr", "127.0.0.1:0"},
		{"-addr", ":8080", "-admin-addr", ":8081"},
		{"-addr", "127.0.0.1:8080", "-admin-addr", "127.0.0.2:8080"},
	} {
		if _, err := Parse(args, env(nil), io.Discard); err != nil {
			t.Errorf("Parse(%v): %v", args, err)
		}
	}
}

func TestTrustedProxies(t *testing.T) {
	c, err := Parse(nil, env(map[string]string{"HOOKWITNESS_TRUSTED_PROXIES": "198.51.100.0/24, 192.0.2.7"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TrustedProxies) != 2 || c.TrustedProxies[1].String() != "192.0.2.7/32" {
		t.Fatalf("trusted proxies %v", c.TrustedProxies)
	}
}

func TestReplayAllow(t *testing.T) {
	c, err := Parse(nil, env(nil), io.Discard)
	if err != nil || c.ReplayAllow.Off() || c.ReplayAllow.String() != "any target except link-local and cloud-metadata addresses" {
		t.Fatalf("default: %v %v", c.ReplayAllow, err)
	}
	c, err = Parse(nil, env(map[string]string{"HOOKWITNESS_REPLAY_ALLOW": "localhost, 198.51.100.0/24"}), io.Discard)
	if err != nil || c.ReplayAllow.String() != "only localhost, 198.51.100.0/24" {
		t.Fatalf("env: %v %v", c.ReplayAllow, err)
	}
	c, err = Parse([]string{"-replay-allow", "none"}, env(map[string]string{"HOOKWITNESS_REPLAY_ALLOW": "localhost"}), io.Discard)
	if err != nil || !c.ReplayAllow.Off() {
		t.Fatalf("flag over env: %v %v", c.ReplayAllow, err)
	}
}
