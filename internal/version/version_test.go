package version

import (
	"runtime/debug"
	"testing"
)

func TestFromBuildInfo(t *testing.T) {
	rev := debug.BuildSetting{Key: "vcs.revision", Value: "5a45bb4c0ffee0123456789abcdef0123456789a"}
	for _, c := range []struct {
		name     string
		version  string
		settings []debug.BuildSetting
		want     string
		commit   string
	}{
		{"go install of a tag", "v0.1.0", nil, "v0.1.0", ""},
		{"go install of an rc", "v0.1.0-rc.1", nil, "v0.1.0-rc.1", ""},
		{"go install of a commit", "v0.1.1-0.20261016120000-abcdef123456", nil, "v0.1.1-0.20261016120000-abcdef123456", "abcdef1"},
		{"go build in a checkout", "v0.1.1-0.20261016120000-abcdef123456+dirty", []debug.BuildSetting{rev}, "v0.1.1-0.20261016120000-abcdef123456+dirty", "5a45bb4"},
		{"a dirty pseudo-version", "v0.1.1-0.20261016120000-abcdef123456+dirty", nil, "v0.1.1-0.20261016120000-abcdef123456+dirty", "abcdef1"},
		{"go build without a module version", "(devel)", []debug.BuildSetting{rev}, "", "5a45bb4"},
		{"nothing recorded", "(devel)", nil, "", ""},
		{"empty", "", nil, "", ""},
		{"a prerelease is not a hash", "v0.2.0-beta-abcdefabcdef", nil, "v0.2.0-beta-abcdefabcdef", ""},
		{"a pseudo-version after a prerelease", "v0.2.0-rc.1.0.20261016120000-abcdef123456", nil, "v0.2.0-rc.1.0.20261016120000-abcdef123456", "abcdef1"},
		{"not hex", "v0.1.1-0.20261016120000-abcdefxyz123", nil, "v0.1.1-0.20261016120000-abcdefxyz123", ""},
		{"a short revision", "v0.1.0", []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}}, "v0.1.0", ""},
	} {
		bi := &debug.BuildInfo{Main: debug.Module{Version: c.version}, Settings: c.settings}
		v, commit := fromBuildInfo(bi)
		if v != c.want || commit != c.commit {
			t.Errorf("%s: got %q %q, want %q %q", c.name, v, commit, c.want, c.commit)
		}
	}
}
