package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestName(t *testing.T) {
	for _, c := range []struct{ v, want string }{
		{"v1.0.0", "hookwitness_1.0.0_linux_arm64"},
		{"v1.0.0-rc.1", "hookwitness_1.0.0-rc.1_linux_arm64"},
		{"edge", "hookwitness_edge_linux_arm64"},
	} {
		if got := Name(c.v, "linux", "arm64"); got != c.want {
			t.Errorf("Name(%q) = %q, want %q", c.v, got, c.want)
		}
	}
}

func testFiles(t *testing.T) []file {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Given out of order: the archive sorts them.
	return []file{
		{name: "hookwitness", path: write("bin", "\x7fELF binary"), mode: 0o755},
		{name: "README.md", path: write("readme", "# Hookwitness\n"), mode: 0o644},
		{name: "LICENSE", path: write("license", "Apache License\n"), mode: 0o644},
	}
}

// The same input gives the same bytes, whatever the files' times and modes
// on disk.
func TestPackReproducible(t *testing.T) {
	mtime := time.Unix(1760000000, 0)
	files := testFiles(t)
	var a, b bytes.Buffer
	if err := Pack(&a, "hookwitness_1.0.0_linux_amd64", files, mtime); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	for _, f := range files {
		os.Chtimes(f.path, later, later)
		os.Chmod(f.path, 0o640)
	}
	if err := Pack(&b, "hookwitness_1.0.0_linux_amd64", files, mtime); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("two packs of the same files differ")
	}
}

func TestPackLayout(t *testing.T) {
	mtime := time.Unix(1760000000, 0)
	var buf bytes.Buffer
	if err := Pack(&buf, "hookwitness_1.0.0_darwin_arm64", testFiles(t), mtime); err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if zr.Name != "" || !zr.ModTime.IsZero() {
		t.Errorf("gzip header carries name %q, time %v", zr.Name, zr.ModTime)
	}
	tr := tar.NewReader(zr)
	var got []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !h.ModTime.Equal(mtime) || h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Errorf("%s: time %v, owner %d:%d %q:%q", h.Name, h.ModTime, h.Uid, h.Gid, h.Uname, h.Gname)
		}
		got = append(got, h.Name+" "+modeString(h.Mode))
		if h.Name == "hookwitness_1.0.0_darwin_arm64/hookwitness" {
			b, _ := io.ReadAll(tr)
			if string(b) != "\x7fELF binary" {
				t.Errorf("binary content %q", b)
			}
		}
	}
	want := []string{
		"hookwitness_1.0.0_darwin_arm64/ 755",
		"hookwitness_1.0.0_darwin_arm64/LICENSE 644",
		"hookwitness_1.0.0_darwin_arm64/README.md 644",
		"hookwitness_1.0.0_darwin_arm64/hookwitness 755",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("entries:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func modeString(m int64) string { return strconv.FormatInt(m&0o777, 8) }

func TestSums(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "b.tar.gz"), []byte("b"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.tar.gz"), []byte("a"), 0o644)
	got, err := Sums(dir, []string{"b.tar.gz", "a.tar.gz"})
	if err != nil {
		t.Fatal(err)
	}
	want := "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb  a.tar.gz\n" +
		"3e23e8160039594a33894f6564e1b1348bbd7a0088d42c4acb73eeaed59c009d  b.tar.gz\n"
	if string(got) != want {
		t.Errorf("SHA256SUMS:\n%s\nwant:\n%s", got, want)
	}
}

func TestRunRefusesAnExistingOut(t *testing.T) {
	out := t.TempDir()
	err := run("v1.0.0", "abc1234", time.Unix(0, 0), "", out)
	if err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("err = %v, want a refusal of the existing directory", err)
	}
}
