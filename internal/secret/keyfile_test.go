package secret

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Servers starting together on a new database all read the same key: each
// makes one, and the first link wins.
func TestKeyFileRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hw.db.key")
	const n = 8
	var arrived sync.WaitGroup
	arrived.Add(n)
	release := make(chan struct{})
	beforeLink = func() { arrived.Done(); <-release } // all ready to link at once
	defer func() { beforeLink = func() {} }()
	keys := make([]*Key, n)
	errs := make([]error, n)
	var done sync.WaitGroup
	for i := range n {
		done.Add(1)
		go func() { defer done.Done(); keys[i], errs[i] = Load("", path) }()
	}
	arrived.Wait()
	close(release)
	done.Wait()
	sealed := keys[0].Seal("x")
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("start %d: %v", i, errs[i])
		}
		if got, err := keys[i].Open(sealed); err != nil || got != "x" {
			t.Fatalf("start %d has another key: %v", i, err)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
}

// A crash between writing the key and linking it leaves no key file, so the
// next start makes a whole one (the old code could leave an empty file that
// blocked every later start).
func TestKeyFileCrash(t *testing.T) {
	if p := os.Getenv("HW_KEY_CRASH"); p != "" {
		beforeLink = func() { os.Exit(3) }
		Load("", p)
		return
	}
	path := filepath.Join(t.TempDir(), "hw.db.key")
	cmd := exec.Command(os.Args[0], "-test.run=^TestKeyFileCrash$")
	cmd.Env = append(os.Environ(), "HW_KEY_CRASH="+path)
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
		t.Fatalf("the crashing start: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a key file after the crash: %v", err)
	}
	if _, err := Load("", path); err != nil {
		t.Fatalf("the next start: %v", err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
}

// Without hard links the key is created in place, and the caller is told
// that it was not atomic.
func TestKeyFileWithoutHardLinks(t *testing.T) {
	linkFile = func(string, string) error { return &os.LinkError{Op: "link", Err: syscall.ENOTSUP} }
	defer func() { linkFile = os.Link }()
	path := filepath.Join(t.TempDir(), "hw.db.key")
	k1, err := Load("", path)
	if err != nil || len(k1.Notes()) != 1 || !strings.Contains(k1.Notes()[0], "without a hard link") {
		t.Fatalf("%v %v", err, k1)
	}
	k2, err := Load("", path)
	if err != nil || len(k2.Notes()) != 0 {
		t.Fatalf("second start: %v %v", err, k2.Notes())
	}
	if got, err := k2.Open(k1.Seal("x")); err != nil || got != "x" {
		t.Fatal("the second start has another key")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
}

// An empty key file (left by an older version's interrupted first start) is
// refused with what to do about it; old temporary files are cleaned up.
func TestKeyFileLeftovers(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "hw.db.key")
	os.WriteFile(empty, nil, 0o600)
	if _, err := Load("", empty); err == nil || !strings.Contains(err.Error(), "is empty: left by an interrupted first start") {
		t.Fatalf("empty: %v", err)
	}
	path := filepath.Join(dir, "other.db.key")
	stale, fresh := path+".tmp-1", path+".tmp-2"
	os.WriteFile(stale, []byte("x"), 0o600)
	os.WriteFile(fresh, []byte("x"), 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(stale, old, old)
	if _, err := Load("", path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a stale temporary key file is still there")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a temporary key file being made right now was removed")
	}
}
