package auth

import (
	"encoding/json"
	"os"
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
