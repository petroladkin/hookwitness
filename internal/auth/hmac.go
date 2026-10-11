package auth

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// HMACParams describe a signature header of any sender.
type HMACParams struct {
	Header   string
	Algo     string // sha1, sha256 or sha512
	Encoding string // hex or base64
	Prefix   string // before the signature, e.g. "sha256="; may be empty
}

// ValidAlgo reports whether the generic check knows the algorithm.
func ValidAlgo(a string) bool { return algos[a] != nil }

// HMAC checks a signature header against the HMAC of content under the
// secret's text. The caller renders content from the check's template (the
// raw body by default); HasBody still gates it, since every template sees
// the body.
func HMAC(in Input, p HMACParams, secret string, content []byte) Verdict {
	if !in.HasBody {
		return noBody()
	}
	got, ok := in.Get(p.Header)
	if !ok {
		return fail("header " + p.Header + " missing")
	}
	sum := mac(p.Algo, []byte(secret), content)
	enc := hex.EncodeToString(sum)
	if p.Encoding == "base64" {
		enc = base64.StdEncoding.EncodeToString(sum)
	}
	sig, found := strings.CutPrefix(got, p.Prefix)
	v := pass()
	switch {
	case !found:
		v = fail(`value does not start with "` + p.Prefix + `"`)
	case p.Encoding == "base64" && !isBase64(sig):
		v = fail("signature is not base64")
	case p.Encoding != "base64" && !isHex(sig):
		v = fail("signature is not hex")
	case p.Encoding == "base64" && !same([]byte(enc), []byte(sig)):
		v = fail("signature does not match")
	case p.Encoding != "base64" && !same([]byte(enc), []byte(strings.ToLower(sig))):
		v = fail("signature does not match")
	}
	v.Expected, v.Received = Short(p.Prefix, enc), got
	return v
}
