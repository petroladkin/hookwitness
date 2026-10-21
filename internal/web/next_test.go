package web

import "testing"

// A login's next page and a form's back page stay on this admin, with the
// admin under a prefix and at the root of its own port: a browser drops
// tabs and newlines from a URL, so "/\t/evil.example" is another site.
func TestNextPageStaysHere(t *testing.T) {
	for _, prefix := range []string{"/admin", ""} {
		h := &Handler{prefix: prefix}
		for _, next := range []string{"/", "/e/1", "/e/1/r/2?replay", "/users?saved=role"} {
			if got := h.nextPage(prefix + next); got != prefix+next {
				t.Errorf("prefix %q: %q went to %q", prefix, prefix+next, got)
			}
		}
		for _, next := range []string{
			"/\t/evil.example", "/\n/evil.example", "/\r/evil.example", "/\x7f/evil.example",
			"//evil.example", "/\\evil.example", "/\\/evil.example", "https://evil.example/",
			"http:/evil.example", "evil.example", "", prefix + "/\t/evil.example", prefix + "/x\x00y",
			prefix + "/login", prefix + "/logout",
		} {
			if got := h.nextPage(next); got != prefix+"/" {
				t.Errorf("prefix %q: %q went to %q", prefix, next, got)
			}
		}
	}
}
