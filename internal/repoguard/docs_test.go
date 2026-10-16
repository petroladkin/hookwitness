package repoguard

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// englishOnly are the pages of docs/ without a Ukrainian twin: the API
// reference is linked from docs/uk instead.
var englishOnly = []string{"api.md"}

// linked are the Markdown files whose relative links must resolve, besides
// docs/ and docs/uk/.
var linked = []string{"README.md", "CHANGELOG.md", "CONTRIBUTING.md", "SECURITY.md", "skills/hookwitness/SKILL.md"}

// TestDocsHaveTwins: every page of docs/ has its Ukrainian twin in docs/uk/
// and the other way round, and the twins carry the same shell examples
// (make docs-check runs the English ones).
func TestDocsHaveTwins(t *testing.T) {
	root := moduleRoot(t)
	en := pages(t, filepath.Join(root, "docs"))
	uk := pages(t, filepath.Join(root, "docs", "uk"))
	for _, p := range en {
		if slices.Contains(englishOnly, p) {
			continue
		}
		if !slices.Contains(uk, p) {
			t.Errorf("docs/%s has no Ukrainian twin docs/uk/%s", p, p)
			continue
		}
		a := shellBlocks(read(t, filepath.Join(root, "docs", p)))
		b := shellBlocks(read(t, filepath.Join(root, "docs", "uk", p)))
		if !slices.Equal(a, b) {
			t.Errorf("docs/%s and docs/uk/%s differ in their sh blocks (%d and %d blocks); the commands must be the same", p, p, len(a), len(b))
		}
	}
	for _, p := range uk {
		if !slices.Contains(en, p) {
			t.Errorf("docs/uk/%s has no English original docs/%s", p, p)
		}
	}
}

// TestDocsLinksResolve: relative links and images in the documentation
// point at files that exist, and their #anchors at headings there.
func TestDocsLinksResolve(t *testing.T) {
	root := moduleRoot(t)
	files := slices.Clone(linked)
	for _, p := range pages(t, filepath.Join(root, "docs")) {
		files = append(files, "docs/"+p)
	}
	for _, p := range pages(t, filepath.Join(root, "docs", "uk")) {
		files = append(files, "docs/uk/"+p)
	}
	for _, f := range files {
		for _, l := range links(read(t, filepath.Join(root, f))) {
			if strings.Contains(l, "://") || strings.HasPrefix(l, "mailto:") {
				continue
			}
			path, anchor, _ := strings.Cut(l, "#")
			target := filepath.Join(root, f)
			if path != "" {
				target = filepath.Join(filepath.Dir(target), path)
			}
			info, err := os.Stat(target)
			if err != nil {
				t.Errorf("%s: link %q: no such file", f, l)
				continue
			}
			if anchor == "" || info.IsDir() || !strings.HasSuffix(target, ".md") {
				continue
			}
			if !slices.Contains(anchors(read(t, target)), anchor) {
				t.Errorf("%s: link %q: no heading with that anchor", f, l)
			}
		}
	}
}

// TestDocsImagesUsed: every file in docs/images is shown somewhere, so the
// media script's output and the pages stay in step.
func TestDocsImagesUsed(t *testing.T) {
	root := moduleRoot(t)
	var all strings.Builder
	for _, p := range pages(t, filepath.Join(root, "docs")) {
		all.WriteString(read(t, filepath.Join(root, "docs", p)))
	}
	all.WriteString(read(t, filepath.Join(root, "README.md")))
	entries, err := os.ReadDir(filepath.Join(root, "docs", "images"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.Contains(all.String(), "images/"+e.Name()) {
			t.Errorf("docs/images/%s is not shown on any English page", e.Name())
		}
	}
}

// pages lists the Markdown files directly in dir (symlinks skipped: the
// private folders linked into docs/ are not documentation).
func pages(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".md") {
			out = append(out, e.Name())
		}
	}
	return out
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// prose returns the lines outside fenced code blocks.
func prose(md string) []string {
	var out []string
	fence := ""
	for _, l := range strings.Split(md, "\n") {
		s := strings.TrimSpace(l)
		if fence == "" && (strings.HasPrefix(s, "```") || strings.HasPrefix(s, "~~~")) {
			fence = s[:3]
			if strings.HasPrefix(s, "````") {
				fence = "````"
			}
			continue
		}
		if fence != "" {
			if strings.HasPrefix(s, fence) && strings.Trim(s, fence[:1]) == "" {
				fence = ""
			}
			continue
		}
		out = append(out, l)
	}
	return out
}

// shellBlocks returns the bodies of the ```sh blocks, in order.
func shellBlocks(md string) []string {
	var out []string
	var cur []string
	in := false
	for _, l := range strings.Split(md, "\n") {
		s := strings.TrimSpace(l)
		switch {
		case !in && s == "```sh":
			in, cur = true, nil
		case in && s == "```":
			in = false
			out = append(out, strings.Join(cur, "\n"))
		case in:
			cur = append(cur, s)
		}
	}
	return out
}

var (
	linkRe = regexp.MustCompile(`\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	codeRe = regexp.MustCompile("`[^`]*`")
)

func links(md string) []string {
	var out []string
	for _, l := range prose(md) {
		for _, m := range linkRe.FindAllStringSubmatch(codeRe.ReplaceAllString(l, ""), -1) {
			out = append(out, m[1])
		}
	}
	return out
}

// anchors are the GitHub anchors of a page's headings: lower case, letters,
// digits, - and _ kept, spaces as -, a repeated one numbered.
func anchors(md string) []string {
	var out []string
	seen := map[string]int{}
	for _, l := range prose(md) {
		h, ok := strings.CutPrefix(l, "#")
		if !ok {
			continue
		}
		h = strings.TrimLeft(h, "#")
		if !strings.HasPrefix(h, " ") {
			continue
		}
		var b strings.Builder
		for _, r := range strings.ToLower(strings.TrimSpace(h)) {
			switch {
			case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
				b.WriteRune(r)
			case r == ' ':
				b.WriteRune('-')
			}
		}
		a := b.String()
		if n := seen[a]; n > 0 {
			out = append(out, a+"-"+strconv.Itoa(n))
		} else {
			out = append(out, a)
		}
		seen[a]++
	}
	return out
}

// TestDocsImageTag: the documentation tells users to run one image tag, a
// release's, never :edge (which the deployment page only explains).
func TestDocsImageTag(t *testing.T) {
	root := moduleRoot(t)
	files := slices.Clone(linked)
	for _, p := range pages(t, filepath.Join(root, "docs")) {
		files = append(files, "docs/"+p)
	}
	for _, p := range pages(t, filepath.Join(root, "docs", "uk")) {
		files = append(files, "docs/uk/"+p)
	}
	image := regexp.MustCompile(`ghcr\.io/petroladkin/hookwitness:([A-Za-z0-9._-]+)`)
	where := map[string][]string{}
	for _, f := range files {
		for i, line := range strings.Split(read(t, filepath.Join(root, f)), "\n") {
			for _, m := range image.FindAllStringSubmatch(line, -1) {
				where[m[1]] = append(where[m[1]], f+":"+strconv.Itoa(i+1))
			}
		}
	}
	if len(where) == 0 {
		t.Fatal("no image reference found in the documentation")
	}
	if len(where) > 1 {
		t.Errorf("the documentation uses %d image tags, one expected: %v", len(where), where)
	}
	if w, ok := where["edge"]; ok {
		t.Errorf("the documentation tells users to run :edge: %v", w)
	}
}
