package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/store"
)

func (e env) listAt(t *testing.T, path string) []map[string]any {
	t.Helper()
	rec := e.do("GET", path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list %s: %d %s", path, rec.Code, rec.Body)
	}
	return decodeAs[struct{ Requests []map[string]any }](t, rec).Requests
}

func listIDs(items []map[string]any) string {
	var out []string
	for _, it := range items {
		out = append(out, fmt.Sprint(it["id"]))
	}
	return "[" + strings.Join(out, " ") + "]"
}

func TestSearchRequests(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	for _, b := range []string{
		`{"taskId":42,"state":"done","ok":true}`,                    // 1
		`{"taskId":"42","state":"failed","ok":false}`,               // 2
		`{"taskId":43,"state":"done","note":null,"tags":["a","b"]}`, // 3
		`{"task":{"id":42,"a/b":"x~y"},"state":"Done"}`,             // 4
		`taskId=42 state=done`,                                      // 5: not JSON
		"\xff\xfetaskId",                                            // 6
	} {
		e.add(t, ep.ID, b, nil)
	}
	e.add(t, ep.ID, "", func(r *store.Request) { r.Body, r.TooLarge, r.Size, r.Verdict = nil, true, 1<<30, "fail" }) // 7
	e.add(t, store.Unmatched, `{"taskId":42}`, func(r *store.Request) { r.Path, r.Status = "/typo", 404 })           // 8
	base := fmt.Sprintf("/endpoints/%d/requests", ep.ID)
	for q, want := range map[string]string{
		"?q=done":                           "[5 3 1]", // case-sensitive: not "Done"
		"?q=Done":                           "[4]",
		"?q=%FF%FE":                         "[6]", // raw bytes
		"?q=taskId&after=2":                 "[3 5 6]",
		"?q=taskId&limit=2":                 "[6 5]",
		"?json=/taskId=42":                  "[2 1]", // a number or a string, as written
		"?json=/taskId=42.0":                "[]",
		"?json=/task/id=42":                 "[4]",
		"?json=/task/a~1b=x~y":              "[4]", // ~1 is /, ~0 is ~; the value is taken as is
		"?json=/ok=true":                    "[1]",
		"?json=/ok=false":                   "[2]",
		"?json=/note=null":                  "[3]",
		"?json=/tags/1=b":                   "[3]",
		"?json=/tags/01=b":                  "[]", // RFC 6901: no leading zero, no sign
		"?json=/tags/+1=b":                  "[]",
		"?json=/tags=a":                     "[]",  // arrays and objects never match
		"?json=/taskId=42&json=/state=done": "[1]", // all must match
		"?json=/taskId=42&q=failed":         "[2]",
		"?json=/state=done&after=0&limit=1": "[1]",
		"?json=/state=done&verdict=fail":    "[]",
		"?q=x&status=200":                   "[4]",
		"?json=/x=":                         "[]",
	} {
		if got := listIDs(e.listAt(t, base+q)); got != want {
			t.Errorf("%s: %s, want %s", q, got, want)
		}
	}
	if got := listIDs(e.listAt(t, "/unmatched/requests?json=/taskId=42")); got != "[8]" {
		t.Errorf("Unmatched: %s", got)
	}
	for q, field := range map[string]string{
		"?q=": "q", "?json=taskId=42": "json", "?json=/taskId": "json", "?include=bodies": "include", "?include=": "include",
	} {
		wantError(t, e.do("GET", base+q, ""), http.StatusUnprocessableEntity, "invalid", field)
	}
}

func TestSearchWaitsForAMatch(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	go func() {
		time.Sleep(100 * time.Millisecond)
		e.add(t, ep.ID, `{"taskId":7}`, nil) // 1: wakes the wait, does not match
		time.Sleep(100 * time.Millisecond)
		e.add(t, ep.ID, `{"taskId":42}`, nil) // 2
	}()
	start := time.Now()
	got := e.listAt(t, fmt.Sprintf("/endpoints/%d/requests?after=0&wait=5s&json=/taskId=42&include=body", ep.ID))
	if listIDs(got) != "[2]" || got[0]["body_text"] != `{"taskId":42}` || time.Since(start) > 3*time.Second {
		t.Fatalf("waited %v for %v", time.Since(start), got)
	}
}

func TestIncludeBody(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	e.add(t, ep.ID, `{"a":1}`, nil)
	e.add(t, ep.ID, "\xff\x00", nil)
	e.add(t, ep.ID, "", nil)
	e.add(t, ep.ID, "", func(r *store.Request) { r.Body, r.TooLarge, r.Size = nil, true, 1<<30 })
	base := fmt.Sprintf("/endpoints/%d/requests", ep.ID)
	got := e.listAt(t, base+"?include=body&after=0")
	want := []struct{ text, b64 any }{
		{`{"a":1}`, nil}, {nil, base64.StdEncoding.EncodeToString([]byte("\xff\x00"))}, {"", nil}, {nil, nil},
	}
	for i, w := range want {
		if got[i]["body_text"] != w.text || got[i]["body_base64"] != w.b64 {
			t.Errorf("request %d: text %v, base64 %v", i+1, got[i]["body_text"], got[i]["body_base64"])
		}
	}
	for _, it := range e.listAt(t, base) {
		if _, ok := it["body_text"]; ok {
			t.Fatal("bodies without include=body")
		}
	}
	for range 110 {
		e.add(t, ep.ID, "{}", nil)
	}
	if n := len(e.listAt(t, base+"?include=body&limit=500")); n != 100 {
		t.Fatalf("with bodies the limit is capped at 100: %d", n)
	}
	if n := len(e.listAt(t, base+"?include=body&limit=7")); n != 7 {
		t.Fatalf("a smaller limit stays: %d", n)
	}
	if n := len(e.listAt(t, base+"?limit=500")); n != 114 {
		t.Fatalf("without bodies the limit stays 500: %d", n)
	}
}

// TestEveryTimestampHasMilliseconds: every time in every JSON answer has
// exactly three fraction digits, whatever precision the stored time has.
func TestEveryTimestampHasMilliseconds(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	e.do("PUT", fmt.Sprintf("/endpoints/%d/secrets/PW", ep.ID), `{"kind":"token","value":"test-pw"}`)
	for i, at := range []time.Time{
		time.Date(2026, 10, 13, 18, 48, 1, 0, time.UTC),
		time.Date(2026, 10, 13, 18, 48, 1, 380_000_000, time.UTC),
		time.Date(2026, 10, 13, 18, 48, 1, 123_456_789, time.FixedZone("EEST", 3*3600)),
	} {
		e.add(t, ep.ID, fmt.Sprintf(`{"n":%d}`, i), func(r *store.Request) {
			r.ReceivedAt = at
			r.Verdict = "pass"
			r.Verdicts = fmt.Sprintf(`[{"check":1,"label":"Basic credentials","result":"pass","reason":"ok","at":%q}]`,
				at.Format(time.RFC3339Nano))
		})
	}
	e.add(t, store.Unmatched, "", nil)
	stampRE := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)
	looksLikeTime := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T`)
	var walk func(where string, v any) int
	walk = func(where string, v any) int {
		n := 0
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				n += walk(where+"/"+k, c)
			}
		case []any:
			for i, c := range x {
				n += walk(fmt.Sprintf("%s/%d", where, i), c)
			}
		case string:
			if looksLikeTime.MatchString(x) {
				if !stampRE.MatchString(x) {
					t.Errorf("%s: %q", where, x)
				}
				n++
			}
		}
		return n
	}
	id := fmt.Sprint(ep.ID)
	calls := []struct{ method, path, body string }{
		{"GET", "/endpoints", ""},
		{"POST", "/endpoints", `{"path":"/new"}`},
		{"GET", "/endpoints/" + id, ""},
		{"PATCH", "/endpoints/" + id, `{"path":"/hooks"}`},
		{"GET", "/endpoints/" + id + "/behaviour", ""},
		{"PUT", "/endpoints/" + id + "/secrets/K", `{"kind":"key","value":"whsec_dGVzdA=="}`},
		{"POST", "/endpoints/" + id + "/reset-counter", "{}"},
		{"POST", "/endpoints/" + id + "/recheck", "{}"},
		{"GET", "/endpoints/" + id + "/requests", ""},
		{"GET", "/endpoints/" + id + "/requests?include=body", ""},
		{"GET", "/requests/1", ""},
		{"GET", "/requests/3", ""},
		{"GET", "/unmatched/requests", ""},
	}
	stamps := 0
	for _, c := range calls {
		rec := e.do(c.method, c.path, c.body)
		if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Header().Get("Content-Type"))
		}
		var v any
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		stamps += walk(c.method+" "+c.path, v)
	}
	if stamps < 15 {
		t.Fatalf("only %d timestamps seen", stamps)
	}
}
