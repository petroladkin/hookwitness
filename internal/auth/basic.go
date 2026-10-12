package auth

import (
	"encoding/base64"
	"strings"
)

// Bearer and Basic never see the stored token or password: only a match
// function, which compares against a hash in constant time. A verdict of
// theirs keeps no values, since the expected one would be the secret.

// credentials returns what follows the scheme in Authorization.
func credentials(in Input, scheme string) (string, Verdict, bool) {
	got, ok := in.Get("Authorization")
	if !ok {
		return "", fail("header Authorization missing"), false
	}
	got = strings.TrimSpace(got)
	s, cred := got, ""
	if i := strings.IndexAny(got, " \t"); i >= 0 {
		s, cred = got[:i], got[i+1:]
	}
	if !strings.EqualFold(s, scheme) {
		return "", fail("scheme is " + s + ", not " + scheme), false
	}
	return strings.TrimSpace(cred), Verdict{}, true
}

// Bearer checks "Authorization: Bearer <token>".
func Bearer(in Input, match func(token string) bool) Verdict {
	tok, v, ok := credentials(in, "Bearer")
	switch {
	case !ok:
		return v
	case tok == "":
		return fail("no token after Bearer")
	case !match(tok):
		return fail("token does not match")
	}
	return Verdict{Result: Pass, Reason: "token matches"}
}

// Basic checks "Authorization: Basic base64(user:password)". The user name
// is not a secret; it is still compared in constant time, and the password
// is always checked, so the time taken does not tell which one was wrong.
func Basic(in Input, user string, match func(password string) bool) Verdict {
	cred, v, ok := credentials(in, "Basic")
	if !ok {
		return v
	}
	raw, err := base64.StdEncoding.DecodeString(cred)
	u, p, found := strings.Cut(string(raw), ":")
	if err != nil || !found {
		return fail("credentials are not base64 user:password")
	}
	okUser, okPassword := same([]byte(u), []byte(user)), match(p)
	if !okUser || !okPassword {
		return fail("user or password does not match")
	}
	return Verdict{Result: Pass, Reason: "user and password match"}
}
