package config

import (
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	c, err := Parse(nil, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Addr: DefaultAddr, DBPath: DefaultDBPath, LogFormat: DefaultLogFormat}
	if c != want {
		t.Fatalf("got %+v, want %+v", c, want)
	}
}

func TestEnvOverridesDefault(t *testing.T) {
	c, err := Parse(nil, env(map[string]string{
		"HOOKWITNESS_ADDR":       "127.0.0.1:9000",
		"HOOKWITNESS_DB":         "/tmp/x.db",
		"HOOKWITNESS_LOG_FORMAT": "json",
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != "127.0.0.1:9000" || c.DBPath != "/tmp/x.db" || c.LogFormat != "json" {
		t.Fatalf("env not applied: %+v", c)
	}
}

func TestFlagWinsOverEnv(t *testing.T) {
	c, err := Parse([]string{"-addr", ":7000", "-log-format", "text"},
		env(map[string]string{"HOOKWITNESS_ADDR": "127.0.0.1:9000", "HOOKWITNESS_LOG_FORMAT": "json"}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":7000" || c.LogFormat != "text" {
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
