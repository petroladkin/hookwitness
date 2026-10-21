package catch

import (
	"strings"
	"testing"
)

func TestCheckPathReserved(t *testing.T) {
	for _, c := range []struct {
		reserved, path string
		refused        bool
	}{
		{"/admin", "/admin", true},
		{"/admin", "/admin/x", true},
		{"/admin", "/administrator", false},
		{"/admin", "/healthz", true},
		{"/admin", "/stripe", false},
		// An admin on its own address reserves nothing on the catch side but
		// the health check.
		{"", "/admin/x", false},
		{"", "/healthz", true},
		{"", "/", false},
		{"", "/stripe/webhook", false},
	} {
		msg := CheckPath(c.reserved, c.path)
		if (msg != "") != c.refused {
			t.Errorf("CheckPath(%q, %q) = %q, refused want %v", c.reserved, c.path, msg, c.refused)
		}
	}
	if msg := CheckPath("", "/healthz"); strings.Contains(msg, "()") || !strings.Contains(msg, "health check") {
		t.Errorf("message names nothing reserved but the health check: %q", msg)
	}
}
