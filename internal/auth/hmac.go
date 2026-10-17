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

// decodeSig decodes a received signature: hex in either case, or base64 in
// any of its forms (standard or URL alphabet, padded or not), since senders
// differ.
func decodeSig(encoding, sig string) ([]byte, bool) {
	if sig == "" {
		return nil, false
	}
	if encoding != "base64" {
		b, err := hex.DecodeString(sig)
		return b, err == nil
	}
	for _, e := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if b, err := e.DecodeString(sig); err == nil {
			return b, true
		}
	}
	return nil, false
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
	raw, ok := decodeSig(p.Encoding, sig)
	switch {
	case !found:
		v = fail(`value does not start with "` + p.Prefix + `"`)
	case !ok && p.Encoding == "base64":
		v = fail("signature is not base64")
	case !ok:
		v = fail("signature is not hex")
	case !same(sum, raw):
		v = fail("signature does not match")
	}
	v.Expected, v.Received = Short(p.Prefix, enc), got
	return v
}
