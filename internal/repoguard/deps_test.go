package repoguard

import (
	"os/exec"
	"strings"
	"testing"
)

// The server is built without test-only code: the test OpenID Connect
// provider (internal/oidc/oidctest, scripts/oidcmock) is not in the
// package graph of cmd/hookwitness.
func TestServerHasNoTestProvider(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./cmd/hookwitness")
	cmd.Dir = moduleRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.Contains(pkg, "/oidctest") || strings.Contains(pkg, "/oidcmock") {
			t.Errorf("cmd/hookwitness depends on %s", pkg)
		}
	}
}
