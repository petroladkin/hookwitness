package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/petroladkin/hookwitness/internal/emulate"
)

func TestSetSecrets(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")

	rec := e.do("PUT", "/endpoints/1/secrets/PASS", `{"kind":"token","value":"first-password"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("token: %d %s", rec.Code, rec.Body)
	}
	s := decodeAs[secretJSON](t, rec)
	if s.Name != "PASS" || s.Kind != "token" || s.Fingerprint == "" || len(s.UsedBy) != 0 {
		t.Fatalf("token %+v", s)
	}
	if rec := e.do("PUT", "/endpoints/1/secrets/GH", `{"kind":"key","value":"whsec_dGVzdC1zaWduaW5nLWtleQ=="}`); rec.Code != http.StatusOK ||
		!decodeAs[secretJSON](t, rec).Standard {
		t.Fatalf("key: %d %s", rec.Code, rec.Body)
	}
	cfg := e.stored(t, ep.ID)
	if len(cfg.Secrets) != 2 || cfg.Secrets[0].Hash == "" || cfg.Secrets[1].Sealed == "" {
		t.Fatalf("stored %+v", cfg.Secrets)
	}

	// Replacing keeps the kind; the kind may be left out.
	rec = e.do("PUT", "/endpoints/1/secrets/PASS", `{"value":"second-password"}`)
	if rec.Code != http.StatusOK || decodeAs[secretJSON](t, rec).Fingerprint == s.Fingerprint {
		t.Fatalf("replace: %d %s", rec.Code, rec.Body)
	}
	wantError(t, e.do("PUT", "/endpoints/1/secrets/PASS", `{"kind":"key","value":"x"}`), http.StatusUnprocessableEntity, "invalid", "kind")
	wantError(t, e.do("PUT", "/endpoints/1/secrets/NEW", `{"value":"x"}`), http.StatusUnprocessableEntity, "invalid", "kind")
	wantError(t, e.do("PUT", "/endpoints/1/secrets/NEW", `{"kind":"password","value":"x"}`), http.StatusUnprocessableEntity, "invalid", "kind")
	wantError(t, e.do("PUT", "/endpoints/1/secrets/NEW", `{"kind":"token","value":""}`), http.StatusUnprocessableEntity, "invalid", "value")
	wantError(t, e.do("PUT", "/endpoints/1/secrets/NEW", `{"kind":"token","value":" padded "}`), http.StatusUnprocessableEntity, "invalid", "value")
	wantError(t, e.do("PUT", "/endpoints/1/secrets/1BAD", `{"kind":"token","value":"x"}`), http.StatusUnprocessableEntity, "invalid", "name")
	wantError(t, e.do("PUT", "/endpoints/9/secrets/X", `{"kind":"token","value":"x"}`), http.StatusNotFound, "not_found", "")
	if n := len(e.stored(t, ep.ID).Secrets); n != 2 {
		t.Fatalf("%d secrets after refusals", n)
	}
}

func TestDeleteSecret(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	e.do("PUT", "/endpoints/1/secrets/PASS", `{"kind":"token","value":"p4ss"}`)
	_, tag := e.behaviour(t, ep.ID)
	if rec := e.putBehaviour(ep.ID, tag, `{"checks":[{"kind":"basic","secret":"PASS","user":"u"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("check: %d %s", rec.Code, rec.Body)
	}
	got := wantError(t, e.do("DELETE", "/endpoints/1/secrets/PASS", ""), http.StatusConflict, "secret_in_use", "")
	if !strings.Contains(got.Error.Message, "check 1 (Basic credentials)") {
		t.Fatalf("message %q", got.Error.Message)
	}
	_, tag = e.behaviour(t, ep.ID)
	e.putBehaviour(ep.ID, tag, `{}`)
	if rec := e.do("DELETE", "/endpoints/1/secrets/PASS", ""); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	wantError(t, e.do("DELETE", "/endpoints/1/secrets/PASS", ""), http.StatusNotFound, "not_found", "")
	if n := len(e.stored(t, ep.ID).Secrets); n != 0 {
		t.Fatalf("%d secrets left", n)
	}
}

// TestSecretValuesNeverLeave sets secrets with distinctive values and looks
// for them in every answer, header and log line the API produced.
func TestSecretValuesNeverLeave(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t, "/hooks")
	values := []string{"leak-canary-token-7Q2", "leak-canary-key-9Z4", "whsec_bGVhay1jYW5hcnktc3RhbmRhcmQ="}
	var seen []*httptest.ResponseRecorder
	keep := func(rec *httptest.ResponseRecorder) { seen = append(seen, rec) }

	keep(e.do("PUT", "/endpoints/1/secrets/TOKEN", `{"kind":"token","value":"`+values[0]+`"}`))
	keep(e.do("PUT", "/endpoints/1/secrets/KEY", `{"kind":"key","value":"`+values[1]+`"}`))
	keep(e.do("PUT", "/endpoints/1/secrets/STD", `{"kind":"key","value":"`+values[2]+`"}`))
	// Refusals that echo what was sent must not echo the value.
	keep(e.do("PUT", "/endpoints/1/secrets/TOKEN", `{"kind":"key","value":"`+values[0]+`"}`))
	keep(e.do("PUT", "/endpoints/1/secrets/9X", `{"kind":"token","value":"`+values[0]+`"}`))
	keep(e.do("PUT", "/endpoints/1/secrets/X", `{"kind":"token","value":"`+values[0]+`","extra":1}`))
	keep(e.do("PUT", "/endpoints/1/secrets/X", `{"kind":"token","value":`+values[0]+`}`))
	keep(e.do("GET", "/endpoints/1/behaviour", ""))
	keep(e.do("GET", "/endpoints", ""))
	keep(e.do("GET", "/endpoints/1", ""))
	_, tag := e.behaviour(t, 1)
	keep(e.putBehaviour(1, tag, `{"checks":[{"kind":"bearer","secret":"TOKEN"},{"kind":"github","secret":"KEY"}]}`))
	keep(e.do("DELETE", "/endpoints/1/secrets/KEY", ""))

	for i, rec := range seen {
		var all strings.Builder
		all.WriteString(rec.Body.String())
		for name, vs := range rec.Header() {
			all.WriteString(name + ": " + strings.Join(vs, ",") + "\n")
		}
		for _, v := range values {
			if strings.Contains(all.String(), v) {
				t.Fatalf("answer %d (status %d) contains a secret value:\n%s", i, rec.Code, all.String())
			}
		}
	}
	for _, v := range values {
		if strings.Contains(e.log.String(), v) {
			t.Fatalf("the log contains a secret value:\n%s", e.log)
		}
	}
	// And the store keeps only sealed and hashed forms.
	for _, s := range e.stored(t, 1).Secrets {
		for _, v := range values {
			if strings.Contains(s.Sealed+s.Hash+s.Fingerprint, v) {
				t.Fatalf("secret %s stored in plain text", s.Name)
			}
		}
	}
	if cfg := e.stored(t, 1); len(cfg.Secrets) != 3 || cfg.Secrets[0].Kind != emulate.SecretToken {
		t.Fatalf("secrets %+v", cfg.Secrets)
	}
}
