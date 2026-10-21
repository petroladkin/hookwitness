// Command dist builds the release archives: one hookwitness binary per
// platform, packed with the license, the third-party notices and the README
// into hookwitness_<version>_<os>_<arch>.tar.gz, and SHA256SUMS over them.
//
// Usage (make dist passes these):
//
//	go run ./scripts/dist -version v1.2.3 -commit abc1234 \
//		-time <unix seconds> -ldflags "<flags>" -out dist
//
// The archives are reproducible: the binaries are built with -trimpath and
// without cgo, every entry has the same time (the commit's, from -time),
// owner 0 and a fixed mode, entries are in a fixed order, and the gzip
// header carries no name or time. The same commit built with the same Go
// release gives byte-identical archives (`make dist` twice, then cmp).
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// platforms are the targets a release ships.
var platforms = []struct{ os, arch string }{
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"darwin", "amd64"},
	{"darwin", "arm64"},
}

// docs travel with every binary: the license and the bundled licenses must.
var docs = []string{"LICENSE", "README.md", "THIRD-PARTY-NOTICES.md"}

func main() {
	version := flag.String("version", "", "the version the binaries report (v1.2.3); names the archives without the v")
	commit := flag.String("commit", "", "the commit the binaries report")
	unix := flag.Int64("time", 0, "the time of every archive entry, in Unix seconds (the commit's)")
	ldflags := flag.String("ldflags", "", "-ldflags for go build")
	out := flag.String("out", "dist", "the directory for the archives and SHA256SUMS; must not exist")
	flag.Parse()
	if err := run(*version, *commit, time.Unix(*unix, 0).UTC(), *ldflags, *out); err != nil {
		fmt.Fprintln(os.Stderr, "dist:", err)
		os.Exit(1)
	}
}

func run(version, commit string, mtime time.Time, ldflags, out string) error {
	if version == "" || commit == "" {
		return errors.New("-version and -commit are required")
	}
	if strings.ContainsAny(version, `/\ `) {
		return fmt.Errorf("%q cannot name a file", version)
	}
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("%s exists: remove it first", out)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	build, err := os.MkdirTemp("", "hookwitness-dist-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(build)

	var names []string
	for _, p := range platforms {
		bin := filepath.Join(build, p.os+"_"+p.arch, "hookwitness")
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", bin, "./cmd/hookwitness")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+p.os, "GOARCH="+p.arch)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("build %s/%s: %w", p.os, p.arch, err)
		}
		files := []file{{name: "hookwitness", path: bin, mode: 0o755}}
		for _, d := range docs {
			files = append(files, file{name: d, path: d, mode: 0o644})
		}
		base := Name(version, p.os, p.arch)
		f, err := os.Create(filepath.Join(out, base+".tar.gz"))
		if err != nil {
			return err
		}
		err = Pack(f, base, files, mtime)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("pack %s: %w", base, err)
		}
		names = append(names, base+".tar.gz")
		fmt.Println(filepath.Join(out, base+".tar.gz"))
	}
	sums, err := Sums(out, names)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "SHA256SUMS"), sums, 0o644)
}

// Name is an archive's name without the extension, which is also the
// directory its files unpack into.
func Name(version, goos, arch string) string {
	return fmt.Sprintf("hookwitness_%s_%s_%s", strings.TrimPrefix(version, "v"), goos, arch)
}

type file struct {
	name string // in the archive, under the directory
	path string // on disk
	mode int64
}

// Pack writes a gzipped tar of files under the directory dir: the
// directory first, then the files sorted by name, every entry with mtime,
// owner 0 and its mode, so the same input gives the same bytes.
func Pack(w io.Writer, dir string, files []file, mtime time.Time) error {
	files = append([]file(nil), files...)
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	zw, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(zw)
	hdr := func(name string, typ byte, mode, size int64) *tar.Header {
		return &tar.Header{Typeflag: typ, Name: name, Mode: mode, Size: size, ModTime: mtime, Format: tar.FormatUSTAR}
	}
	if err := tw.WriteHeader(hdr(dir+"/", tar.TypeDir, 0o755, 0)); err != nil {
		return err
	}
	for _, f := range files {
		b, err := os.ReadFile(f.path)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(hdr(dir+"/"+f.name, tar.TypeReg, f.mode, int64(len(b)))); err != nil {
			return err
		}
		if _, err := io.Copy(tw, bytes.NewReader(b)); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// Sums is SHA256SUMS over the named files of dir, in the format sha256sum
// -c reads: the hex digest, two spaces, the name; sorted by name.
func Sums(dir string, names []string) ([]byte, error) {
	names = append([]string(nil), names...)
	sort.Strings(names)
	var b bytes.Buffer
	for _, n := range names {
		f, err := os.Open(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "%x  %s\n", h.Sum(nil), n)
	}
	return b.Bytes(), nil
}
