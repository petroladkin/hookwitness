package emulate

import "testing"

// Protect drops a reply's Set-Cookie of any admin cookie, the provider
// sign-in's flow cookie too; others stay.
func TestProtectAdminCookies(t *testing.T) {
	r := Rendered{Headers: []Header{{"Set-Cookie", "hookwitness_session=x; Path=/"}, {"Set-Cookie", "hookwitness_oidc=y"}, {"set-cookie", "app=1"}}}
	got := r.Protect("hookwitness_session", "hookwitness_oidc")
	if v := headerValues(got.Headers, "Set-Cookie"); len(v) != 1 || v[0] != "app=1" {
		t.Fatalf("Set-Cookie kept: %v", v)
	}
}
