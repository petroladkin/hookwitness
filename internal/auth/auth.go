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
	NotChecked    Result = "n/c" // the check does not apply: no schema for the event's type
)

// Verdict is the result of one check on one request.
type Verdict struct {
	Check    int       `json:"check"` // 1-based position in the endpoint's checks
	Label    string    `json:"label"` // "GitHub signature", "Bearer token"
	Result   Result    `json:"result"`
	Reason   string    `json:"reason"`
	Expected string    `json:"expected,omitempty"` // signatures only, truncated (Short)
	Received string    `json:"received,omitempty"` // signatures only, as received
	Problems []Problem `json:"problems,omitempty"` // schema checks: the first problems found
	Total    int       `json:"total,omitempty"`    // schema checks: all problems found
	Images   []Image   `json:"images,omitempty"`   // schema checks: the images the schema declares, as found
	At       time.Time `json:"at"`                 // when it was computed
}

// Problem is one way a body breaks its schema.
type Problem struct {
	Path    string `json:"path"`    // JSON pointer into the body; "" for the whole body
	Keyword string `json:"keyword"` // the failing keyword: required, type, format... or json
	Message string `json:"message"`
}

// Image is a picture a schema declares in a body (a base64 string or a data
// URL marked with contentMediaType image/...), as found there.
type Image struct {
	Path     string `json:"path"`            // JSON pointer of the string
	Declared string `json:"declared"`        // the schema's contentMediaType
	Type     string `json:"type,omitempty"`  // sniffed: image/png...; "" when not an image
	Size     int    `json:"size"`            // decoded bytes
	Width    int    `json:"width,omitempty"` // pixels; 0 when not decoded here
	Height   int    `json:"height,omitempty"`
	SHA256   string `json:"sha256,omitempty"` // of the decoded bytes
	Error    string `json:"error,omitempty"`  // why it is not a valid image of its declared type
	// NotChecked says why its pixels were not decoded (it would take too
	// much memory); the header was checked.
	NotChecked string `json:"not_checked,omitempty"`
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
