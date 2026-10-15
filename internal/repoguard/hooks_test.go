package repoguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hookRepo is a throwaway repository on a feature branch with its own
// deny-list (a synthetic pattern), so the real repository's list is never
// read.
type hookRepo struct {
	t   *testing.T
	dir string
	env []string
}

const hookPattern = "zz-hookguard-[0-9]+"

func newHookRepo(t *testing.T) *hookRepo {
	t.Helper()
	for _, tool := range []string{"git", "bash", "file"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("the hook test needs %s: %v", tool, err)
		}
	}
	r := &hookRepo{t: t, dir: t.TempDir()}
	// Inherited GIT_* variables (set when this runs inside a hook) would point
	// the commands at the outer repository.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			r.env = append(r.env, kv)
		}
	}
	r.env = append(r.env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	r.git("init", "-q", "-b", "feature/t")
	gitDir := strings.TrimSpace(r.git("rev-parse", "--git-dir"))
	r.write(filepath.Join(gitDir, "info", "private-patterns"), hookPattern+"\n")
	return r
}

func (r *hookRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = r.dir, r.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (r *hookRepo) write(name, content string) {
	r.t.Helper()
	p := filepath.Join(r.dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// commit stores what is given as the base state, without the hook.
func (r *hookRepo) commit(files map[string]string) {
	r.t.Helper()
	for n, c := range files {
		r.write(n, c)
	}
	r.git("add", "-A")
	r.git("commit", "-q", "--no-verify", "-m", "base")
}

// hook runs the pre-commit hook on what is staged; it reports whether the
// commit would be allowed, and its message.
func (r *hookRepo) hook(t *testing.T) (bool, string) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(moduleRoot(t), "scripts", "hooks", "pre-commit"))
	cmd.Dir, cmd.Env = r.dir, r.env
	out, err := cmd.CombinedOutput()
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		t.Fatalf("running the hook: %v", err)
	}
	return err == nil, string(out)
}

// status is the staged change's status letters, as git reports them.
func (r *hookRepo) status() string {
	var letters []string
	for _, line := range strings.Split(strings.TrimSpace(r.git("diff", "--cached", "--name-status")), "\n") {
		if line != "" {
			letters = append(letters, line[:1])
		}
	}
	return strings.Join(letters, "")
}

func TestPreCommitHook(t *testing.T) {
	// Reading the hook makes go test's cache depend on it: the hook only
	// runs in a child process, which the cache does not see.
	if _, err := os.ReadFile(filepath.Join(moduleRoot(t), "scripts", "hooks", "pre-commit")); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable() // a real compiled binary of this platform
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Repeat("an ordinary line of notes\n", 20)
	for _, c := range []struct {
		name    string
		base    map[string]string
		change  func(r *hookRepo)
		status  string
		allowed bool
	}{
		{"add a clean file", nil, func(r *hookRepo) { r.write("a.txt", "fine\n"); r.git("add", "a.txt") }, "A", true},
		{"add a private line", nil, func(r *hookRepo) { r.write("a.txt", "see zz-hookguard-42\n"); r.git("add", "a.txt") }, "A", false},
		{"modify with a private line", map[string]string{"a.txt": notes}, func(r *hookRepo) {
			r.write("a.txt", notes+"zz-hookguard-7\n")
			r.git("add", "a.txt")
		}, "M", false},
		{"rename with a private line", map[string]string{"notes.txt": notes}, func(r *hookRepo) {
			r.git("mv", "notes.txt", "moved.txt")
			r.write("moved.txt", notes+"zz-hookguard-1\n")
			r.git("add", "moved.txt")
		}, "R", false},
		{"copy with a private line", map[string]string{"notes.txt": notes}, func(r *hookRepo) {
			// git reports a copy (C) only with diff.renames=copies and only
			// from a source changed in the same commit.
			r.git("config", "diff.renames", "copies")
			r.write("notes.txt", notes+"one more ordinary line\n")
			r.write("copy.txt", notes+"zz-hookguard-2\n")
			r.git("add", "notes.txt", "copy.txt")
		}, "CM", false},
		{"a pure rename", map[string]string{"notes.txt": notes}, func(r *hookRepo) { r.git("mv", "notes.txt", "moved.txt") }, "R", true},
		{"a binary moved to the root", map[string]string{"bin/tool": string(binary)}, func(r *hookRepo) { r.git("mv", "bin/tool", "tool") }, "R", false},
		{"a binary added to the root", nil, func(r *hookRepo) { r.write("tool", string(binary)); r.git("add", "tool") }, "A", false},
		{"a deletion", map[string]string{"a.txt": "zz-hookguard-3\n"}, func(r *hookRepo) { r.git("rm", "-q", "a.txt") }, "D", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newHookRepo(t)
			r.commit(map[string]string{"README": "base\n"})
			if c.base != nil {
				r.commit(c.base)
			}
			c.change(r)
			// The case must stage what it says, or it would test nothing.
			if got := r.status(); got != c.status {
				t.Fatalf("staged %q, want %q", got, c.status)
			}
			allowed, out := r.hook(t)
			if allowed != c.allowed {
				t.Fatalf("allowed %v, want %v:\n%s", allowed, c.allowed, out)
			}
		})
	}
}
