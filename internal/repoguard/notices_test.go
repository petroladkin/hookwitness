package repoguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestNoticesCoverEveryModule checks THIRD-PARTY-NOTICES.md against the
// modules actually compiled into the binary: every one must have a section,
// and no section may outlive its module. `make notices` regenerates the file.
func TestNoticesCoverEveryModule(t *testing.T) {
	root := repoRoot(t)
	cmd := exec.Command("go", "list", "-deps", "-f", "{{with .Module}}{{if not .Main}}{{.Path}}{{end}}{{end}}", "./cmd/hookwitness")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	deps := map[string]bool{}
	for _, m := range strings.Fields(string(out)) {
		deps[m] = true
	}

	notices, err := os.ReadFile(filepath.Join(root, "THIRD-PARTY-NOTICES.md"))
	if err != nil {
		t.Fatal(err)
	}
	listed := listedModules(string(notices))

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
