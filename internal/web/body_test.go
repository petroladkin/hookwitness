package web

import (
	"strings"
	"testing"
)

func TestViewBody(t *testing.T) {
	cases := []struct {
		body string
		kind bodyKind
	}{
		{"", bodyEmpty},
		{`{"a":1}`, bodyJSON},
		{"a=1&b=2", bodyText},
		{"привіт, світ\n", bodyText},
		{"<xml/>", bodyText},
		{"\x00\x01binary", bodyBinary},
		{"\xff\xfe", bodyBinary},
		{`{"a":`, bodyText}, // broken JSON is still text
	}
	for _, c := range cases {
		if got := viewBody([]byte(c.body)).Kind; got != c.kind {
			t.Errorf("%q: kind %d, want %d", c.body, got, c.kind)
		}
	}
	big := viewBody([]byte(strings.Repeat("a", previewLimit+10)))
	if big.Kind != bodyText || !big.Truncated || len(big.Text) != previewLimit {
		t.Errorf("large text: kind %d truncated %v len %d", big.Kind, big.Truncated, len(big.Text))
	}
	// Cut inside a multi-byte character: still text.
	cut := viewBody([]byte(strings.Repeat("a", previewLimit-1) + "é"))
	if cut.Kind != bodyText {
		t.Errorf("text cut in a character became kind %d", cut.Kind)
	}
	bin := viewBody(make([]byte, 2000))
	if bin.Kind != bodyBinary || !bin.Truncated || strings.Count(bin.Hex, "\n") != hexLimit/16 {
		t.Errorf("binary preview: %d lines, truncated %v", strings.Count(bin.Hex, "\n"), bin.Truncated)
	}
}

func TestSize(t *testing.T) {
	for n, want := range map[int64]string{-1: "unknown size", 0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 10 << 20: "10.0 MiB"} {
		if got := size(n); got != want {
			t.Errorf("size(%d) = %q, want %q", n, got, want)
		}
	}
}
