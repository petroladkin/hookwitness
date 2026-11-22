// Package release tests scripts/release/draft.sh, the binaries job's step
// that puts the archives on the tag's draft Release, against a fake gh.
package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGH is a gh that answers the releases query with drafts (one .draft
// value per line) and records every other call.
const fakeGH = `#!/bin/bash
if [ "$1" = api ]; then printf '%s' "$FAKE_DRAFTS"; exit 0; fi
echo "gh $*" >> "$FAKE_CALLS"
`

func TestDraftStep(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGH), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, tag, drafts string
		ok                bool
		call, says        string
	}{
		{"no Release: a draft is made", "v0.3.0", "", true, "gh release create v0.3.0 --draft --verify-tag", ""},
		{"a release candidate: a prerelease draft", "v0.3.0-rc.1", "", true, "gh release create v0.3.0-rc.1 --draft --prerelease", ""},
		{"one draft: uploaded again", "v0.3.0", "true\n", true, "gh release upload v0.3.0 dist/a.tar.gz dist/SHA256SUMS --clobber", ""},
		{"published: left alone", "v0.3.0", "false\n", false, "", "the Release of v0.3.0 is already published"},
		{"two drafts: refused", "v0.3.0", "true\ntrue\n", false, "", "2 Releases carry tag v0.3.0 (drafts: 2); delete the extra drafts by hand"},
		{"published and a draft: refused", "v0.3.0", "false\ntrue\n", false, "", "2 Releases carry tag v0.3.0 (drafts: 1)"},
	} {
		calls := filepath.Join(t.TempDir(), "calls")
		cmd := exec.Command("bash", "draft.sh", c.tag, "dist/a.tar.gz", "dist/SHA256SUMS")
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_DRAFTS="+c.drafts, "FAKE_CALLS="+calls,
			"GITHUB_REPOSITORY=owner/repo")
		out, err := cmd.CombinedOutput()
		made, _ := os.ReadFile(calls)
		if (err == nil) != c.ok {
			t.Errorf("%s: exit %v\n%s", c.name, err, out)
		}
		if c.call == "" && len(made) > 0 {
			t.Errorf("%s: gh was asked to change a Release: %s", c.name, made)
		}
		if c.call != "" && !strings.Contains(string(made), c.call) {
			t.Errorf("%s: calls %q, want %q", c.name, made, c.call)
		}
		if c.says != "" && !strings.Contains(string(out), c.says) {
			t.Errorf("%s: output %q, want %q", c.name, out, c.says)
		}
	}
}
