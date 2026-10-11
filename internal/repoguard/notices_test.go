package repoguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// platforms is the set the notices cover; the module graph differs per OS, so
// the union is checked whatever OS runs the test. Keep in step with
// scripts/notices.sh.
var platforms = []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "freebsd/amd64"}

// TestNoticesCoverEveryModule checks THIRD-PARTY-NOTICES.md against the
// modules compiled into the binary on any supported platform: every one must have a section,
// and no section may outlive its module. `make notices` regenerates the file.
func TestNoticesCoverEveryModule(t *testing.T) {
	root := moduleRoot(t)
	deps := map[string]bool{}
	for _, p := range platforms {
		goos, goarch, _ := strings.Cut(p, "/")
		cmd := exec.Command("go", "list", "-deps", "-f", "{{with .Module}}{{if not .Main}}{{.Path}}{{end}}{{end}}", "./cmd/hookwitness")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list for %s: %v", p, err)
		}
		for _, m := range strings.Fields(string(out)) {
			deps[m] = true
		}
	}

	notices, err := os.ReadFile(filepath.Join(root, "THIRD-PARTY-NOTICES.md"))
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for h := range listedModules(string(notices)) {
		if !strings.HasPrefix(h, assetDir) {
			listed[h] = true
		}
	}

	var missing, stale []string
	for m := range deps {
		if !listed[m] {
			missing = append(missing, m)
		}
	}
	for m := range listed {
		if !deps[m] {
			stale = append(stale, m)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("modules compiled into the binary without a notice: %v (run make notices)", missing)
	}
	if len(stale) > 0 {
		t.Errorf("notices for modules no longer compiled in: %v (run make notices)", stale)
	}
}

// assetDir holds the vendored web assets, one directory each.
const assetDir = "internal/web/static/vendor/"

// TestNoticesCoverEveryBundledAsset checks the bundled web assets the same
// way: each vendored directory carries its LICENSE and SOURCE and has a
// section in THIRD-PARTY-NOTICES.md, and no section outlives its directory.
func TestNoticesCoverEveryBundledAsset(t *testing.T) {
	root := moduleRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, assetDir))
	if err != nil {
		t.Fatal(err)
	}
	dirs := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			t.Errorf("%s%s: vendored files go in a directory with their LICENSE and SOURCE", assetDir, e.Name())
			continue
		}
		dir := assetDir + e.Name()
		dirs[dir] = true
		for _, f := range []string{"LICENSE", "SOURCE"} {
			if _, err := os.Stat(filepath.Join(root, dir, f)); err != nil {
				t.Errorf("%s has no %s", dir, f)
			}
		}
	}
	notices, err := os.ReadFile(filepath.Join(root, "THIRD-PARTY-NOTICES.md"))
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for h := range listedModules(string(notices)) {
		if strings.HasPrefix(h, assetDir) {
			listed[h] = true
		}
	}
	for d := range dirs {
		if !listed[d] {
			t.Errorf("bundled asset %s has no notice (run make notices)", d)
		}
	}
	for d := range listed {
		if !dirs[d] {
			t.Errorf("notice for %s, which is no longer bundled (run make notices)", d)
		}
	}
}

// moduleRoot is the directory holding go.mod; unlike the private-string scan
// this check needs no git, so it also runs in an unpacked source archive.
func moduleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		t.Fatal("not inside the hookwitness module")
	}
	return filepath.Dir(gomod)
}

// listedModules returns the level-2 headings outside fenced blocks: the
// license texts are quoted inside fences and carry headings of their own.
func listedModules(md string) map[string]bool {
	listed := map[string]bool{}
	fence := ""
	for _, line := range strings.Split(md, "\n") {
		if fence != "" {
			if strings.TrimSpace(line) == fence {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(line, "```") {
			fence = strings.TrimRight(line, "abcdefghijklmnopqrstuvwxyz")
			continue
		}
		if m, ok := strings.CutPrefix(line, "## "); ok {
			listed[strings.Fields(m)[0]] = true
		}
	}
	return listed
}
