package repoguard

import (
	"bufio"
	"bytes"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

const (
	allowLine = "guard:allow"
	allowFile = "guard:allow-file"
)

var (
	// Candidate IPv4 literals, not glued to other digits or dots (so version
	// strings such as 1.10.0.1.2 are not read as addresses).
	ipv4 = regexp.MustCompile(`(?:^|[^0-9.])((?:[0-9]{1,3}\.){3}[0-9]{1,3})(?:$|[^0-9.]|\.[^0-9])`)
	// Absolute paths into a user's home directory on macOS or Linux.
	homePath = regexp.MustCompile(`/(?:[U]sers|home)/[A-Za-z0-9._-]+/`)
)

// Finding is one offending line.
type Finding struct {
	File string
	Line int
	What string
	Text string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", f.File, f.Line, f.What, strings.TrimSpace(f.Text))
}

// Scan reports private network addresses and home-directory paths in data,
// honouring the guard:allow and guard:allow-file markers. Binary content
// (a NUL byte in the first 8000 bytes) is skipped.
func Scan(name string, data []byte) []Finding {
	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return nil
	}
	first, _, _ := bytes.Cut(data, []byte("\n"))
	if bytes.Contains(first, []byte(allowFile)) {
		return nil
	}
	var out []Finding
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if strings.Contains(line, allowLine) {
			continue
		}
		for _, m := range ipv4.FindAllStringSubmatch(line, -1) {
			if a, err := netip.ParseAddr(m[1]); err == nil && a.IsPrivate() {
				out = append(out, Finding{name, n, "private network address " + m[1], line})
			}
		}
		if homePath.MatchString(line) {
			out = append(out, Finding{name, n, "home directory path", line})
		}
	}
	return out
}
