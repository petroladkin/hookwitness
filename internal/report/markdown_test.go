package report

import "testing"

// mdText keeps running text on one line: a line break in a reason would end
// the list item or paragraph it is in.
func TestMdTextNoLineBreaks(t *testing.T) {
	for in, want := range map[string]string{
		"a\nb":        "a b",
		"a\r\nb":      "a b",
		"a\rb":        "a b",
		"*x*\n# head": `\*x\* # head`,
		"one line":    "one line",
	} {
		if got := mdText(in); got != want {
			t.Errorf("mdText(%q) = %q, want %q", in, got, want)
		}
	}
}
