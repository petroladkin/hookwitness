package access

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"time"
)

// CookieName is the browser's login cookie.
const CookieName = "hookwitness_session"

// Session lifetimes: a login ends after a day without activity, and after a
// week whatever happens.
const (
	IdleLimit     = 24 * time.Hour
	AbsoluteLimit = 7 * 24 * time.Hour
	touchEvery    = time.Minute // activity is recorded at most this often
)

// NewSessionID returns a new session id for the cookie and the hash the
// store keeps.
func NewSessionID() (id string, hash []byte) {
	b := make([]byte, 32)
	rand.Read(b)
	id = base64.RawURLEncoding.EncodeToString(b)
	return id, HashSession(id)
}

// HashSession is the stored form of a session id: its SHA-256. The id has
// full entropy, so a slow hash would add nothing.
func HashSession(id string) []byte {
	h := sha256.Sum256([]byte(id))
	return h[:]
}

// validSessionID reports whether a cookie value can be a session id (43
// base64url characters), so junk costs no database lookup.
func validSessionID(v string) bool {
	if len(v) != 43 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(v)
	return err == nil
}

// Expired reports whether a session created at created and last active at
// seen has ended at now.
func Expired(created, seen, now time.Time) bool {
	return now.Sub(seen) > IdleLimit || now.Sub(created) > AbsoluteLimit
}

// CookiePath is the cookie's Path for an admin prefix: the browser then
// sends it only to the admin ("/" when the admin is at the root of its own
// listener).
func CookiePath(prefix string) string { return prefix + "/" }

// Cookie is the login cookie holding id (an empty id clears it). It lives
// as long as the browser session; the server enforces the limits.
func Cookie(id, prefix string, secure bool) *http.Cookie {
	c := &http.Cookie{
		Name:     CookieName,
		Value:    id,
		Path:     CookiePath(prefix),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	}
	if id == "" {
		c.MaxAge = -1
	}
	return c
}
