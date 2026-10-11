package auth

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
)

// StandardKey decodes a Standard Webhooks secret (base64 after "whsec_")
// into the HMAC key.
func StandardKey(secret string) ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil || len(k) == 0 {
		return nil, errors.New(`the secret is not "whsec_" and base64`)
	}
	return k, nil
}

// Standard checks Standard Webhooks headers (webhook-id, webhook-timestamp,
// webhook-signature) or Svix's (svix-…): each space-separated "v1,<base64>"
// in the signature header is the HMAC-SHA256 of "<id>.<timestamp>.<raw
// body>" under the decoded secret; any one matching passes.
func Standard(in Input, secret string, tol Tolerance) Verdict {
	if !in.HasBody {
		return noBody()
	}
	prefix := "webhook-"
	if _, ok := in.Get("webhook-signature"); !ok {
		if _, svix := in.Get("svix-id"); svix {
			prefix = "svix-"
		} else if _, svix := in.Get("svix-signature"); svix {
			prefix = "svix-"
		}
	}
	var vals [3]string
	for i, n := range []string{"signature", "id", "timestamp"} {
		v, ok := in.Get(prefix + n)
		if !ok {
			return fail("header " + prefix + n + " missing")
		}
		vals[i] = v
	}
	header, id, ts := vals[0], vals[1], vals[2]
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || !plausible(t) {
		return fail("header " + prefix + "timestamp is not a unix time")
	}
	key, err := StandardKey(secret)
	if err != nil {
		return fail(err.Error())
	}
	sum := base64.StdEncoding.EncodeToString(mac("sha256", key, []byte(id+"."+ts+"."+string(in.Body))))
	v := fail("no v1 signature in " + prefix + "signature")
	for _, s := range strings.Fields(header) {
		sig, ok := strings.CutPrefix(s, "v1,")
		if !ok {
			continue
		}
		if v.Result != Pass {
			v = fail("signature does not match")
		}
		if same([]byte(sum), []byte(sig)) {
			v = pass()
		}
	}
	if why := tol.stale(t, in.Received); why != "" && v.Result == Pass {
		v = fail(why)
	}
	v.Expected, v.Received = Short("v1,", sum), header
	return v
}
