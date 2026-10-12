package auth

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// vectors are the golden vectors in testdata/auth (protected): published by
// the providers or computed independently with openssl.
type vectors struct {
	GitHub []struct {
		Name, Secret, Body, Header string
	} `json:"github"`
	Standard []struct {
		Name, Secret, ID, Timestamp, Body, Signature string
	} `json:"standard"`
	Stripe []struct {
		Name, Secret, Timestamp, Body, V1 string
	} `json:"stripe"`
	HMAC []struct {
		Name, Secret, Body, Header, Algo, Encoding, Prefix, Value string
	} `json:"hmac"`
}

func load(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("../../testdata/auth/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// in is a request with a body and header pairs, received 10 s after the
// Stripe vector's timestamp.
func in(body string, headers ...string) Input {
	x := Input{Body: []byte(body), HasBody: true, Received: time.Unix(1760200010, 0)}
	for i := 0; i+1 < len(headers); i += 2 {
		x.Headers = append(x.Headers, Header{headers[i], headers[i+1]})
	}
	return x
}

type outcome struct {
	result Result
	reason string
}

func check(t *testing.T, name string, got Verdict, want outcome) {
	t.Helper()
	if got.Result != want.result || got.Reason != want.reason {
		t.Errorf("%s: got %s %q, want %s %q", name, got.Result, got.Reason, want.result, want.reason)
	}
}

func TestGitHub(t *testing.T) {
	v := load(t).GitHub[0]
	const h = "X-Hub-Signature-256"
	for _, c := range []struct {
		name string
		in   Input
		want outcome
	}{
		{"published vector", in(v.Body, h, v.Header), outcome{Pass, "signature matches"}},
		{"header name in any case", in(v.Body, "x-hub-signature-256", v.Header), outcome{Pass, "signature matches"}},
		{"missing", in(v.Body), outcome{Fail, "header X-Hub-Signature-256 missing"}},
		{"no prefix", in(v.Body, h, v.Header[7:]), outcome{Fail, `header X-Hub-Signature-256 is not "sha256=<hex>"`}},
		{"short", in(v.Body, h, "sha256=757107ea"), outcome{Fail, `header X-Hub-Signature-256 is not "sha256=<hex>"`}},
		{"not hex", in(v.Body, h, "sha256="+v.Header[7:70]+"z"), outcome{Fail, `header X-Hub-Signature-256 is not "sha256=<hex>"`}},
		{"other body", in(v.Body+"!", h, v.Header), outcome{Fail, "signature does not match"}},
		{"no body kept", Input{Headers: []Header{{h, v.Header}}}, outcome{NotVerifiable, "body not kept (over the size limit)"}},
	} {
		check(t, c.name, GitHub(c.in, v.Secret), c.want)
	}
	got := GitHub(in(v.Body, h, "sha256=00"), v.Secret)
	if got.Expected != "sha256=757107ea…" || got.Received != "sha256=00" {
		t.Errorf("evidence: expected %q received %q", got.Expected, got.Received)
	}
}

func TestShort(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"sha256=", "757107ea0eb2"}: "sha256=757107ea…",
		{"", "abc"}:                 "abc",
		{"v1,", "12345678"}:         "v1,12345678",
	} {
		if got := Short(in[0], in[1]); got != want {
			t.Errorf("Short(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

func TestStripe(t *testing.T) {
	v := load(t).Stripe[0]
	ok := "t=" + v.Timestamp + ",v1=" + v.V1
	tol := Tolerance{Seconds: 300}
	zeros := "0000000000000000000000000000000000000000000000000000000000000000"
	for _, c := range []struct {
		name, header string
		received     int64 // unix seconds; the vector is signed at 1760200000
		tol          Tolerance
		want         outcome
	}{
		{"vector", ok, 1760200010, tol, outcome{Pass, "signature matches"}},
		{"spaces after commas", "t=" + v.Timestamp + ", v1=" + v.V1, 1760200010, tol, outcome{Pass, "signature matches"}},
		{"second v1 matches", "t=" + v.Timestamp + ",v1=" + zeros + ",v1=" + v.V1, 1760200010, tol, outcome{Pass, "signature matches"}},
		{"v0 is not v1", "t=" + v.Timestamp + ",v0=" + v.V1, 1760200010, tol, outcome{Fail, "no v1 signature in Stripe-Signature"}},
		{"missing", "", 1760200010, tol, outcome{Fail, "header Stripe-Signature missing"}},
		{"no timestamp", "v1=" + v.V1, 1760200010, tol, outcome{Fail, "no t= timestamp in Stripe-Signature"}},
		{"bad timestamp", "t=soon,v1=" + v.V1, 1760200010, tol, outcome{Fail, "no t= timestamp in Stripe-Signature"}},
		{"mismatch", "t=" + v.Timestamp + ",v1=" + zeros, 1760200010, tol, outcome{Fail, "signature does not match"}},
		{"signed for another time", "t=1760200001,v1=" + v.V1, 1760200010, tol, outcome{Fail, "signature does not match"}},
		{"old", ok, 1760200412, tol, outcome{Fail, "timestamp 412 s old (tolerance 300 s)"}},
		{"old and wrong: the signature is the reason", "t=" + v.Timestamp + ",v1=" + zeros, 1760200412, tol, outcome{Fail, "signature does not match"}},
		{"at the tolerance", ok, 1760200300, tol, outcome{Pass, "signature matches"}},
		{"future", ok, 1760199500, tol, outcome{Fail, "timestamp 500 s in the future (tolerance 300 s)"}},
		{"old but ignored", ok, 1760299999, Tolerance{Ignore: true}, outcome{Pass, "signature matches"}},
		{"zero tolerance means 300 s", ok, 1760200299, Tolerance{}, outcome{Pass, "signature matches"}},
	} {
		x := in(v.Body)
		if c.header != "" {
			x = in(v.Body, "Stripe-Signature", c.header)
		}
		x.Received = time.Unix(c.received, 0)
		check(t, c.name, Stripe(x, v.Secret, c.tol), c.want)
	}
	check(t, "no body kept", Stripe(Input{Headers: []Header{{"Stripe-Signature", ok}}}, v.Secret, tol),
		outcome{NotVerifiable, "body not kept (over the size limit)"})
	got := Stripe(in(v.Body, "Stripe-Signature", "t="+v.Timestamp+",v1="+zeros), v.Secret, tol)
	if got.Expected != "t="+v.Timestamp+",v1=c4bcbd5f…" || got.Received != "t="+v.Timestamp+",v1="+zeros {
		t.Errorf("evidence: expected %q received %q", got.Expected, got.Received)
	}
}

func TestStandard(t *testing.T) {
	v := load(t).Standard[0]
	tol := Tolerance{Seconds: 300}
	headers := func(prefix, sig string) []string {
		return []string{prefix + "id", v.ID, prefix + "timestamp", v.Timestamp, prefix + "signature", sig}
	}
	other := "v1,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	for _, c := range []struct {
		name     string
		headers  []string
		received int64 // the vector is signed at 1614265330
		tol      Tolerance
		want     outcome
	}{
		{"published vector", headers("webhook-", v.Signature), 1614265340, tol, outcome{Pass, "signature matches"}},
		{"svix headers", headers("svix-", v.Signature), 1614265340, tol, outcome{Pass, "signature matches"}},
		{"several signatures", headers("webhook-", other+" "+v.Signature), 1614265340, tol, outcome{Pass, "signature matches"}},
		{"unknown version only", headers("webhook-", "v1a,"+v.Signature[3:]), 1614265340, tol, outcome{Fail, "no v1 signature in webhook-signature"}},
		{"mismatch", headers("webhook-", other), 1614265340, tol, outcome{Fail, "signature does not match"}},
		{"missing id", headers("webhook-", v.Signature)[2:], 1614265340, tol, outcome{Fail, "header webhook-id missing"}},
		{"missing svix id", headers("svix-", v.Signature)[2:], 1614265340, tol, outcome{Fail, "header svix-id missing"}},
		{"missing timestamp", append(headers("webhook-", v.Signature)[:2], headers("webhook-", v.Signature)[4:]...), 1614265340, tol, outcome{Fail, "header webhook-timestamp missing"}},
		{"nothing at all", nil, 1614265340, tol, outcome{Fail, "header webhook-signature missing"}},
		{"timestamp not a number", []string{"webhook-id", v.ID, "webhook-timestamp", "now", "webhook-signature", v.Signature}, 1614265340, tol, outcome{Fail, "header webhook-timestamp is not a unix time"}},
		{"old", headers("webhook-", v.Signature), 1614265330 + 301, tol, outcome{Fail, "timestamp 301 s old (tolerance 300 s)"}},
		{"future", headers("webhook-", v.Signature), 1614265330 - 400, tol, outcome{Fail, "timestamp 400 s in the future (tolerance 300 s)"}},
		{"old but ignored", headers("webhook-", v.Signature), 1714265330, Tolerance{Ignore: true}, outcome{Pass, "signature matches"}},
	} {
		x := in(v.Body, c.headers...)
		x.Received = time.Unix(c.received, 0)
		check(t, c.name, Standard(x, v.Secret, c.tol), c.want)
	}
	x := in(v.Body, headers("webhook-", v.Signature)...)
	x.Received = time.Unix(1614265340, 0)
	check(t, "not a whsec_ secret", Standard(x, "whsec_***", tol), outcome{Fail, `the secret is not "whsec_" and base64`})
	check(t, "no body kept", Standard(Input{Headers: x.Headers, Received: x.Received}, v.Secret, tol),
		outcome{NotVerifiable, "body not kept (over the size limit)"})
	x = in(v.Body, headers("webhook-", other)...)
	x.Received = time.Unix(1614265340, 0)
	if got := Standard(x, v.Secret, tol); got.Expected != "v1,g0hM9SsE…" || got.Received != other {
		t.Errorf("evidence: expected %q received %q", got.Expected, got.Received)
	}
	if _, err := StandardKey("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"); err != nil {
		t.Error(err)
	}
	if _, err := StandardKey("not base64!"); err == nil {
		t.Error("a key that is not base64 accepted")
	}
}

func TestHMAC(t *testing.T) {
	vs := load(t)
	gh := vs.GitHub[0]
	asGitHub := HMACParams{Header: "X-Hub-Signature-256", Algo: "sha256", Encoding: "hex", Prefix: "sha256="}
	check(t, "GitHub through the generic check", HMAC(in(gh.Body, asGitHub.Header, gh.Header), asGitHub, gh.Secret, []byte(gh.Body)),
		outcome{Pass, "signature matches"})
	for _, v := range vs.HMAC {
		p := HMACParams{Header: v.Header, Algo: v.Algo, Encoding: v.Encoding, Prefix: v.Prefix}
		check(t, v.Name, HMAC(in(v.Body, v.Header, v.Value), p, v.Secret, []byte(v.Body)), outcome{Pass, "signature matches"})
		check(t, v.Name+", other content", HMAC(in(v.Body, v.Header, v.Value), p, v.Secret, []byte(v.Body+" ")), outcome{Fail, "signature does not match"})
	}
	sha1 := vs.HMAC[0]
	p := HMACParams{Header: sha1.Header, Algo: "sha1", Encoding: "hex", Prefix: "sha1="}
	b64 := vs.HMAC[1]
	pb := HMACParams{Header: b64.Header, Algo: "sha512", Encoding: "base64"}
	for _, c := range []struct {
		name string
		v    Verdict
		want outcome
	}{
		{"missing", HMAC(in(sha1.Body), p, sha1.Secret, []byte(sha1.Body)), outcome{Fail, "header X-Hub-Signature missing"}},
		{"prefix absent", HMAC(in(sha1.Body, p.Header, sha1.Value[5:]), p, sha1.Secret, []byte(sha1.Body)), outcome{Fail, `value does not start with "sha1="`}},
		{"not hex", HMAC(in(sha1.Body, p.Header, "sha1=xyz"), p, sha1.Secret, []byte(sha1.Body)), outcome{Fail, "signature is not hex"}},
		{"not base64", HMAC(in(b64.Body, pb.Header, "***"), pb, b64.Secret, []byte(b64.Body)), outcome{Fail, "signature is not base64"}},
		{"wrong secret", HMAC(in(sha1.Body, p.Header, sha1.Value), p, "other", []byte(sha1.Body)), outcome{Fail, "signature does not match"}},
		{"no body kept", HMAC(Input{Headers: []Header{{p.Header, sha1.Value}}}, p, sha1.Secret, nil), outcome{NotVerifiable, "body not kept (over the size limit)"}},
	} {
		check(t, c.name, c.v, c.want)
	}
	if got := HMAC(in(sha1.Body, p.Header, "sha1=00"), p, sha1.Secret, []byte(sha1.Body)); got.Expected != "sha1=01dc10d0…" || got.Received != "sha1=00" {
		t.Errorf("evidence: expected %q received %q", got.Expected, got.Received)
	}
	if !ValidAlgo("sha512") || ValidAlgo("md5") {
		t.Error("ValidAlgo")
	}
}

func equals(want string) func(string) bool { return func(got string) bool { return got == want } }

func TestBearer(t *testing.T) {
	tok := equals("tok_test_123")
	for _, c := range []struct {
		name, header string
		want         outcome
	}{
		{"match", "Bearer tok_test_123", outcome{Pass, "token matches"}},
		{"scheme in any case", "bearer tok_test_123", outcome{Pass, "token matches"}},
		{"missing", "", outcome{Fail, "header Authorization missing"}},
		{"wrong scheme", "Basic dTpw", outcome{Fail, "scheme is Basic, not Bearer"}},
		{"no token", "Bearer", outcome{Fail, "no token after Bearer"}},
		{"mismatch", "Bearer tok_test_124", outcome{Fail, "token does not match"}},
	} {
		x := in("")
		if c.header != "" {
			x = in("", "Authorization", c.header)
		}
		x.HasBody = false // bearer does not need the body
		got := Bearer(x, tok)
		check(t, c.name, got, c.want)
		if got.Expected != "" || got.Received != "" {
			t.Errorf("%s: a token check must not keep values", c.name)
		}
	}
}

func TestBasic(t *testing.T) {
	pw := equals("pw_test")
	cred := func(s string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, c := range []struct {
		name, header string
		want         outcome
	}{
		{"match", cred("qa:pw_test"), outcome{Pass, "user and password match"}},
		{"colon in the password", "", outcome{}},
		{"missing", "", outcome{Fail, "header Authorization missing"}},
		{"wrong scheme", "Bearer x", outcome{Fail, "scheme is Bearer, not Basic"}},
		{"not base64", "Basic ***", outcome{Fail, "credentials are not base64 user:password"}},
		{"no colon", cred("qa"), outcome{Fail, "credentials are not base64 user:password"}},
		{"wrong user", cred("ops:pw_test"), outcome{Fail, "user or password does not match"}},
		{"wrong password", cred("qa:pw"), outcome{Fail, "user or password does not match"}},
	} {
		if c.name == "colon in the password" {
			x := in("", "Authorization", cred("qa:a:b"))
			check(t, c.name, Basic(x, "qa", equals("a:b")), outcome{Pass, "user and password match"})
			continue
		}
		x := in("")
		if c.header != "" {
			x = in("", "Authorization", c.header)
		}
		check(t, c.name, Basic(x, "qa", pw), c.want)
	}
}

// TestTimestampOverflow: a timestamp chosen so that received - t wraps
// around must not slip past the tolerance (found in review).
func TestTimestampOverflow(t *testing.T) {
	v := load(t).Stripe[0]
	sw := load(t).Standard[0]
	for _, ts := range []string{"-9223372036854775808", "9223372036854775807", "-9223372034094575798", "0", "-1", "253402300800"} {
		x := in(v.Body, "Stripe-Signature", "t="+ts+",v1="+v.V1)
		check(t, "stripe t="+ts, Stripe(x, v.Secret, Tolerance{Seconds: 300}), outcome{Fail, "no t= timestamp in Stripe-Signature"})
		y := in(sw.Body, "webhook-id", sw.ID, "webhook-timestamp", ts, "webhook-signature", sw.Signature)
		check(t, "standard t="+ts, Standard(y, sw.Secret, Tolerance{Seconds: 300}), outcome{Fail, "header webhook-timestamp is not a unix time"})
	}
}

func TestStripeRepeatedTimestamp(t *testing.T) {
	v := load(t).Stripe[0]
	// The last t= wins, as in a simple key=value parse; the signature was made for the first.
	x := in(v.Body, "Stripe-Signature", "t="+v.Timestamp+",t=1760200001,v1="+v.V1)
	check(t, "repeated t", Stripe(x, v.Secret, Tolerance{Seconds: 300}), outcome{Fail, "signature does not match"})
}

func TestParserLeniency(t *testing.T) {
	check(t, "bearer after a tab", Bearer(in("", "Authorization", "Bearer\ttok_test_123"), equals("tok_test_123")), outcome{Pass, "token matches"})
	v := load(t).HMAC[1] // sha512 base64
	p := HMACParams{Header: v.Header, Algo: v.Algo, Encoding: "base64"}
	std, _ := base64.StdEncoding.DecodeString(v.Value)
	for name, enc := range map[string]string{
		"URL alphabet":    base64.URLEncoding.EncodeToString(std),
		"no padding":      base64.RawStdEncoding.EncodeToString(std),
		"URL, no padding": base64.RawURLEncoding.EncodeToString(std),
	} {
		check(t, name, HMAC(in(v.Body, v.Header, enc), p, v.Secret, []byte(v.Body)), outcome{Pass, "signature matches"})
	}
	gh := load(t).GitHub[0]
	ph := HMACParams{Header: "X-Sig", Algo: "sha256", Encoding: "hex", Prefix: "sha256="}
	check(t, "upper-case hex", HMAC(in(gh.Body, "X-Sig", strings.ToUpper(gh.Header[:7])[:0]+"sha256="+strings.ToUpper(gh.Header[7:])), ph, gh.Secret, []byte(gh.Body)), outcome{Pass, "signature matches"})
}
