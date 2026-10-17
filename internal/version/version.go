// Package version holds the build identity, set at link time:
//
//	go build -ldflags "-X github.com/petroladkin/hookwitness/internal/version.Version=v0.1.0 \
//	  -X github.com/petroladkin/hookwitness/internal/version.Commit=abc1234"
//
// A build without them (go install, a plain go build) reports what Go
// recorded in the binary: the module version and the commit.
package version

import (
	"runtime/debug"
	"strings"
)

var (
	// Version is the release version, "dev" for an untagged build.
	Version = "dev"
	// Commit is the short commit hash the binary was built from.
	Commit = "none"
)

func init() {
	if Version != "dev" {
		return
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	v, c := fromBuildInfo(bi)
	if v != "" {
		Version = v
	}
	if Commit == "none" && c != "" {
		Commit = c
	}
}

// fromBuildInfo returns the module version Go recorded ("" when it has
// none) and the short commit: the VCS revision, else the one a
// pseudo-version ends with, else "".
func fromBuildInfo(bi *debug.BuildInfo) (version, commit string) {
	v := bi.Main.Version
	if v == "(devel)" {
		v = ""
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			return v, s.Value[:7]
		}
	}
	// v0.1.1-0.20261016120000-abcdef123456, perhaps with +dirty.
	base, _, _ := strings.Cut(v, "+")
	parts := strings.Split(base, "-")
	if len(parts) >= 3 {
		h, ts := parts[len(parts)-1], parts[len(parts)-2]
		if len(h) == 12 && isOf(h, "0123456789abcdef") && len(ts) >= 14 && isOf(ts[len(ts)-14:], "0123456789") {
			return v, h[:7]
		}
	}
	return v, ""
}

func isOf(s, digits string) bool {
	for _, r := range s {
		if !strings.ContainsRune(digits, r) {
			return false
		}
	}
	return true
}

// String returns "<version> (<commit>)".
func String() string { return Version + " (" + Commit + ")" }
