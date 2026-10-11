package auth

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultTolerance is how far a signed timestamp may be from the request's
// arrival when a check does not say.
const DefaultTolerance = 300

// Tolerance bounds how far a signed timestamp may be from the time the
// request was received. Judging against the arrival, not the present, keeps
// a re-check later giving the same answer.
type Tolerance struct {
	Seconds int64 // 0 means DefaultTolerance
	Ignore  bool  // accept any timestamp (replayed captures)
}

// stale says why a signed unix timestamp is too far from the arrival, or "".
func (t Tolerance) stale(ts int64, received time.Time) string {
	if t.Ignore {
		return ""
	}
	limit := t.Seconds
	if limit <= 0 {
		limit = DefaultTolerance
	}
	switch d := received.Unix() - ts; {
	case d > limit:
		return fmt.Sprintf("timestamp %d s old (tolerance %d s)", d, limit)
	case -d > limit:
		return fmt.Sprintf("timestamp %d s in the future (tolerance %d s)", -d, limit)
	}
	return ""
}

// Stripe checks Stripe-Signature: t=<unix>,v1=<hex>[,v1=...]. Each v1 is
// the HMAC-SHA256 of "<t>.<raw body>" under the whole whsec_ secret; any
// one matching passes; other schemes (v0) are ignored. A matching signature
// with a timestamp outside the tolerance fails with its age.
func Stripe(in Input, secret string, tol Tolerance) Verdict {
	const name = "Stripe-Signature"
	if !in.HasBody {
		return noBody()
	}
	got, ok := in.Get(name)
	if !ok {
		return fail("header " + name + " missing")
	}
	var ts string
	var sigs []string
	for _, part := range strings.Split(got, ",") {
		k, val, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = val
		case "v1":
			sigs = append(sigs, val)
		}
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fail("no t= timestamp in " + name)
	}
	if len(sigs) == 0 {
		return fail("no v1 signature in " + name)
	}
	sum := hex.EncodeToString(mac("sha256", []byte(secret), []byte(ts+"."+string(in.Body))))
	v := fail("signature does not match")
	for _, s := range sigs {
		if same([]byte(sum), []byte(strings.ToLower(s))) {
			v = pass()
		}
	}
	if why := tol.stale(t, in.Received); why != "" && v.Result == Pass {
		v = fail(why)
	}
	v.Expected, v.Received = Short("t="+ts+",v1=", sum), got
	return v
}
