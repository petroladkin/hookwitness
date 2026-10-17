package auth

import (
	"encoding/hex"
	"strings"
)

// GitHub checks X-Hub-Signature-256: "sha256=" and the hex HMAC-SHA256 of
// the raw body under the webhook secret. Without the body nothing can be
// said, even about a present header.
func GitHub(in Input, secret string) Verdict {
	const name = "X-Hub-Signature-256"
	if !in.HasBody {
		return noBody()
	}
	got, ok := in.Get(name)
	if !ok {
		return fail("header " + name + " missing")
	}
	sum := hex.EncodeToString(mac("sha256", []byte(secret), in.Body))
	sig, found := strings.CutPrefix(got, "sha256=")
	v := pass()
	switch {
	case !found || len(sig) != len(sum) || !isHex(sig):
		v = fail(`header ` + name + ` is not "sha256=<hex>"`)
	case !same([]byte(sum), []byte(strings.ToLower(sig))):
		v = fail("signature does not match")
	}
	v.Expected, v.Received = Short("sha256=", sum), got
	return v
}
