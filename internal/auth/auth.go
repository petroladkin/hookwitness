// Package auth verifies how a caught request is signed or authorized:
// GitHub, Stripe and Standard Webhooks signatures, a generic HMAC header,
// bearer tokens and basic credentials. It is pure: the caller passes the
// request as received and the secret's value; every verifier returns a
// verdict with a one-line reason. Signatures and secrets are compared in
// constant time.
package auth

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"hash"
	"strings"
	"time"
)

// Result of a check.
type Result string

const (
	Pass          Result = "pass"
	Fail          Result = "fail"
	NotVerifiable Result = "n/v" // the request lacks what the check needs (a body not kept)
)

// Verdict is the result of one check on one request.
type Verdict struct {
	Check    int       `json:"check"` // 1-based position in the endpoint's checks
	Label    string    `json:"label"` // "GitHub signature", "Bearer token"
	Result   Result    `json:"result"`
	Reason   string    `json:"reason"`
	Expected string    `json:"expected,omitempty"` // signatures only, truncated (Short)
	Received string    `json:"received,omitempty"` // signatures only, as received
	At       time.Time `json:"at"`                 // when it was computed
}

// Header is one request header line as received.
type Header struct{ Name, Value string }

// Input is what a verifier sees of a request.
type Input struct {
	Headers  []Header
	Body     []byte
	HasBody  bool      // false when the body was not kept (413)
	Received time.Time // signed timestamps are judged against this, not now
}

// Get is the first value of a header, and whether it is present.
func (in Input) Get(name string) (string, bool) {
	for _, h := range in.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value, true
		}
	}
	return "", false
}

// Algorithms of the generic HMAC check.
var algos = map[string]func() hash.Hash{"sha1": sha1.New, "sha256": sha256.New, "sha512": sha512.New}

// mac is the HMAC of msg under key.
func mac(algo string, key, msg []byte) []byte {
	m := hmac.New(algos[algo], key)
	m.Write(msg)
	return m.Sum(nil)
}

// same compares two signatures or tokens in constant time.
func same(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// Short is what a verdict keeps of an expected signature: the first 8
// characters of the signature part. It tells a mismatch apart without
// handing out a valid signature for a body the viewer chose.
func Short(prefix, sig string) string {
	if len(sig) <= 8 {
		return prefix + sig
	}
	return prefix + sig[:8] + "…"
}

func pass() Verdict           { return Verdict{Result: Pass, Reason: "signature matches"} }
func fail(why string) Verdict { return Verdict{Result: Fail, Reason: why} }
func noBody() Verdict {
	return Verdict{Result: NotVerifiable, Reason: "body not kept (over the size limit)"}
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil && s != ""
}

func isBase64(s string) bool {
	_, err := base64.StdEncoding.DecodeString(s)
	return err == nil && s != ""
}
