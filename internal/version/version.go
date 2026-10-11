// Package version holds the build identity, set at link time:
//
//	go build -ldflags "-X github.com/petroladkin/hookwitness/internal/version.Version=v0.1.0 \
//	  -X github.com/petroladkin/hookwitness/internal/version.Commit=abc1234"
package version

var (
	// Version is the release version, "dev" for an untagged build.
	Version = "dev"
	// Commit is the short commit hash the binary was built from.
	Commit = "none"
)

// String returns "<version> (<commit>)".
func String() string { return Version + " (" + Commit + ")" }
