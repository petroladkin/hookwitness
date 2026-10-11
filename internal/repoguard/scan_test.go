package repoguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The offending samples are assembled at run time so that this file does not
// itself contain what it looks for.
func join(parts ...string) string { return strings.Join(parts, "") }

func TestScanFindsAndExempts(t *testing.T) {
	ip10 := join("10", ".1.2.3")
	ip172 := join("172", ".20.0.9")
	ip192 := join("192", ".168.4.5")
	mac := join("/", "Users", "/someone/project")
	linux := join("/", "home", "/someone/.config")

	cases := []struct {
		name string
		in   string
		want int
	}{
		{"10/8", "url = http://" + ip10 + ":8080/x", 1},
		{"172.16/12", "addr " + ip172, 1},
		{"192.168/16", ip192, 1},
		{"macOS home", "see " + mac, 1},
		{"Linux home", "cp x " + linux, 1},
		{"two on a line", ip10 + " and " + ip192, 2},
		{"public address", "198.51.100.7 and 8.8.8.8", 0},
		{"loopback", "127.0.0.1:8080", 0},
		{"172 outside the private block", "172.32.0.1", 0},
		{"version string", "v1." + ip10 + ".4", 0},
		{"not an address", "10.300.1.1", 0},
		{"line marker", ip10 + " // " + allowLine + " fixture", 0},
		{"/home without a user part", "GET /home/ HTTP/1.1", 0},
	}
	for _, tc := range cases {
		if got := Scan("f", []byte(tc.in)); len(got) != tc.want {
			t.Errorf("%s: %d findings %v, want %d", tc.name, len(got), got, tc.want)
		}
	}

	file := "# " + allowFile + "\n" + ip10 + "\n" + mac + "\n"
	if got := Scan("f", []byte(file)); len(got) != 0 {
		t.Errorf("file marker: %v", got)
	}
	notFirst := "first\n# " + allowFile + "\n" + ip10 + "\n"
	if got := Scan("f", []byte(notFirst)); len(got) != 1 {
		t.Errorf("file marker below the first line must not exempt: %v", got)
	}
	if got := Scan("f", []byte("\x00"+ip10)); got != nil {
		t.Errorf("binary content scanned: %v", got)
	}
	got := Scan("a.go", []byte("x\ny = \""+ip192+"\"\n"))
	if len(got) != 1 || got[0].Line != 2 || !strings.HasPrefix(got[0].String(), "a.go:2: ") {
		t.Errorf("finding position: %v", got)
	}
}

// repoRoot returns the top of the git checkout, or skips the test outside
// one: without git there is no list of tracked files to check, and passing
// silently would claim a check that never ran.
func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not a git checkout (git rev-parse: %v); the repository guard did not run", err)
	}
	return strings.TrimSpace(string(out))
}

// TestTrackedFilesHaveNoPrivateStrings is the guard: every file git tracks
// (or has staged) is scanned.
func TestTrackedFilesHaveNoPrivateStrings(t *testing.T) {
	root := repoRoot(t)
	cmd := exec.Command("git", "ls-files", "-z", "--cached")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var findings []Finding
	files := 0
	for _, name := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			if os.IsNotExist(err) {
				continue // deleted in the working tree, not yet staged
			}
			t.Fatal(err)
		}
		files++
		findings = append(findings, Scan(name, data)...)
	}
	if files == 0 {
		t.Fatal("no tracked files found; the guard scanned nothing")
	}
	for _, f := range findings {
		t.Error(f)
	}
	if len(findings) > 0 {
		t.Logf("mark a deliberate line with %q, or a whole fixture with %q in its first line", allowLine, allowFile)
	}
}
