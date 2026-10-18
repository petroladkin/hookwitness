package access

import (
	"crypto/rand"
	"regexp"
	"strings"
)

// TokenPattern matches a personal token anywhere in text: for secret
// scanners and log filters (documented).
const TokenPattern = `hkw_[a-z2-7]{8}_[a-z2-7]{40}`

var tokenRE = regexp.MustCompile(`^` + TokenPattern + `$`)

const base32Lower = "abcdefghijklmnopqrstuvwxyz234567"

func randomBase32(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = base32Lower[b[i]&31]
	}
	return string(b)
}

// MintToken makes a new personal token: hkw_<id>_<secret>, the id 8 and the
// secret 40 base32 characters (200 bits). The id finds the stored record;
// the whole value is what is hashed.
func MintToken() (token, publicID string) {
	publicID = randomBase32(8)
	return "hkw_" + publicID + "_" + randomBase32(40), publicID
}

// ParseToken returns a personal token's public id, or false when value is
// not one.
func ParseToken(value string) (publicID string, ok bool) {
	if !tokenRE.MatchString(value) {
		return "", false
	}
	return value[4:12], true
}

// Shown is how a token is shown after it was created: its id, never the
// secret.
func Shown(publicID string) string { return "hkw_" + publicID + "_…" }

// bearer returns the token of an "Authorization: Bearer <token>" header;
// present reports a Bearer header at all. Other schemes are not ours (a
// proxy's Basic login in front of the admin) and are ignored.
func bearer(header string) (token string, present bool) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(header), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// TokenRE matches a personal token anywhere in text.
func TokenRE() *regexp.Regexp { return tokenAnywhere }

var tokenAnywhere = regexp.MustCompile(TokenPattern)
