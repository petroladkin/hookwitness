package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

func failed(r *store.Request) {
	r.Status = 500
	r.Outcome = emulate.Outcome{Action: emulate.ActReply, Status: 500}.String()
}

// TestRetentionAPI: an endpoint's retention is read, set, applied at once
// and reset; the answer says how many requests went; invalid policies
// change nothing.
func TestRetentionAPI(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/keep")
	for i := range 12 {
		mod := func(r *store.Request) {
			r.Outcome = emulate.Outcome{Action: emulate.ActReply, Status: 200}.String()
		}
		if i%4 == 0 {
			mod = failed
		}
		e.add(t, ep.ID, fmt.Sprint(i), mod)
	}
	got := decodeAs[endpointJSON](t, e.do("GET", fmt.Sprintf("/endpoints/%d", ep.ID), ""))
	if got.Retention != nil || got.RetentionEffective.Normal.Count != 1000 || got.RetentionEffective.Anomalies == nil ||
		got.Kept != (keptJSON{Normal: 9, Anomalies: 3}) {
		t.Fatalf("before: %+v", got)
	}
	path := fmt.Sprintf("/endpoints/%d", ep.ID)
	rec := e.do("PATCH", path, `{"retention":{"normal":{"count":2},"anomalies":{"count":1,"max_age":"7d"}}}`)
	got = decodeAs[endpointJSON](t, rec)
	if rec.Code != http.StatusOK || got.ToPrune == nil || *got.ToPrune != 9 || got.Kept != (keptJSON{Normal: 2, Anomalies: 1}) ||
		got.Pruned.Count != 9 || got.Pruned.NormalUntil == nil || got.Pruned.AnomaliesUntil == nil {
		t.Fatalf("after lowering: %d %s", rec.Code, rec.Body)
	}
	if a := got.RetentionEffective.Anomalies; a == nil || a.Count != 1 || a.MaxAge == nil || *a.MaxAge != "7d" {
		t.Fatalf("effective anomalies %+v", a)
	}
	// Invalid policies change nothing.
	for _, bad := range []string{`{"retention":{"normal":{"count":0}}}`, `{"retention":{"normal":{"max_age":"30m"}}}`,
		`{"retention":{"normal":{"max_age":"1y"}}}`, `{"retention":{"spare":{}}}`, `{"retention":"7d"}`,
		`{"retention":{"normal":{"count":2000000}}}`} {
		wantError(t, e.do("PATCH", path, bad), http.StatusUnprocessableEntity, "invalid", "retention")
	}
	if g := decodeAs[endpointJSON](t, e.do("GET", path, "")); g.Retention == nil || g.Retention.Normal.Count != 2 {
		t.Fatalf("an invalid PATCH changed the policy: %+v", g.Retention)
	}
	// null: back to the defaults.
	got = decodeAs[endpointJSON](t, e.do("PATCH", path, `{"retention":null}`))
	if got.Retention != nil || *got.ToPrune != 0 || got.RetentionEffective.Normal.Count != 1000 {
		t.Fatalf("reset: %+v", got)
	}
	if !bytes.Contains([]byte(e.log.String()), []byte("retention set")) {
		t.Fatalf("no log line:\n%s", e.log)
	}
}

// TestPinsAndFilters: pin and unpin, and the pinned / anomaly filters.
func TestPinsAndFilters(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/pins")
	ok := e.add(t, ep.ID, "ok", nil)
	bad := e.add(t, ep.ID, "bad", failed)
	if rec := e.do("PUT", fmt.Sprintf("/requests/%d/pin", ok.ID), "", "Content-Type", "application/json"); rec.Code != http.StatusNoContent {
		t.Fatalf("pin: %d %s", rec.Code, rec.Body)
	}
	list := func(q string) []int64 {
		var ids []int64
		for _, r := range decodeAs[struct{ Requests []summaryJSON }](t, e.do("GET", fmt.Sprintf("/endpoints/%d/requests%s", ep.ID, q), "")).Requests {
			ids = append(ids, r.ID)
		}
		return ids
	}
	if got := list("?pinned=true"); fmt.Sprint(got) != fmt.Sprint([]int64{ok.ID}) {
		t.Fatalf("pinned=true: %v", got)
	}
	if got := list("?anomaly=true"); fmt.Sprint(got) != fmt.Sprint([]int64{bad.ID}) {
		t.Fatalf("anomaly=true: %v", got)
	}
	r := decodeAs[requestJSON](t, e.do("GET", fmt.Sprintf("/requests/%d", ok.ID), ""))
	if !r.Pinned || r.Anomaly {
		t.Fatalf("request %+v", r.summaryJSON)
	}
	e.do("DELETE", fmt.Sprintf("/requests/%d/pin", ok.ID), "")
	if got := list("?pinned=true"); len(got) != 0 {
		t.Fatalf("after unpinning: %v", got)
	}
	wantError(t, e.do("PUT", "/requests/999/pin", "", "Content-Type", "application/json"), http.StatusNotFound, "not_found", "")
	wantError(t, e.do("GET", fmt.Sprintf("/endpoints/%d/requests?pinned=yes", ep.ID), ""), http.StatusUnprocessableEntity, "invalid", "pinned")
}

// TestUnmatchedSettings: the Unmatched register's summary and its one lane.
func TestUnmatchedSettings(t *testing.T) {
	e := newEnv(t)
	for i := range 5 {
		e.add(t, store.Unmatched, fmt.Sprint(i), func(r *store.Request) { r.Status = 404 })
	}
	got := decodeAs[unmatchedJSON](t, e.do("GET", "/unmatched", ""))
	if got.Requests != 5 || got.RetentionEffective.Anomalies != nil || got.Kept.Normal != 5 {
		t.Fatalf("unmatched: %+v", got)
	}
	got = decodeAs[unmatchedJSON](t, e.do("PATCH", "/unmatched", `{"retention":{"normal":{"count":2}}}`))
	if got.Requests != 2 || got.ToPrune == nil || *got.ToPrune != 3 {
		t.Fatalf("after lowering: %+v", got)
	}
	wantError(t, e.do("PATCH", "/unmatched", `{"retention":{"anomalies":{"count":3}}}`), http.StatusUnprocessableEntity, "invalid", "retention")
	wantError(t, e.do("PATCH", "/unmatched", `{}`), http.StatusUnprocessableEntity, "invalid", "retention")

	// The answers match their schemas.
	c := compiler(t)
	for _, tc := range []struct{ method, path, body, schema string }{
		{"GET", "/unmatched", "", "Unmatched"},
		{"PATCH", "/unmatched", `{"retention":null}`, "Unmatched"},
		{"GET", "/endpoints", "", ""},
	} {
		if tc.schema == "" {
			continue
		}
		sch, err := c.Compile(docURL + "#/components/schemas/" + tc.schema)
		if err != nil {
			t.Fatal(err)
		}
		rec := e.do(tc.method, tc.path, tc.body)
		v, err := jsonschema.UnmarshalJSON(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		if err := sch.Validate(v); err != nil {
			t.Errorf("%s %s does not match %s: %v", tc.method, tc.path, tc.schema, err)
		}
	}
	ep := e.endpoint(t, "/schema")
	e.add(t, ep.ID, "x", nil)
	sch, _ := c.Compile(docURL + "#/components/schemas/Endpoint")
	for _, body := range []string{`{"retention":{"normal":{"count":5,"max_age":"36h"},"anomalies":{"count":0}}}`, `{"retention":null}`} {
		rec := e.do("PATCH", fmt.Sprintf("/endpoints/%d", ep.ID), body)
		v, _ := jsonschema.UnmarshalJSON(bytes.NewReader(rec.Body.Bytes()))
		if err := sch.Validate(v); err != nil {
			t.Errorf("PATCH %s answer does not match Endpoint: %v\n%s", body, err, rec.Body)
		}
	}
}

// TestDeliveryAPI: the behaviour takes an event source and the delivery
// checks; requests carry event and attempt; event= lists an event's attempts.
func TestDeliveryAPI(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/gh")
	path := fmt.Sprintf("/endpoints/%d/behaviour", ep.ID)
	tag := e.do("GET", path, "").Header().Get("ETag")
	body := `{"default":{"status":200},"event":{"from":"header","key":"X-GitHub-Delivery"},
		"checks":[{"kind":"duplicates"},{"kind":"backoff","first_s":5,"max_s":3600,"max_attempts":8},
		{"kind":"order","order":{"from":"body","key":"/seq","as":"number"},"group":{"from":"body","key":"/obj"}}]}`
	if rec := e.do("PUT", path, body, "If-Match", tag); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	var compact bytes.Buffer
	json.Compact(&compact, e.do("GET", path, "").Body.Bytes())
	got := compact.String()
	for _, want := range []string{`"event":{"from":"header","key":"X-GitHub-Delivery"}`, `"kind":"backoff"`, `"first_s":5`, `"as":"number"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("behaviour lacks %s: %s", want, got)
		}
	}
	tag = e.do("GET", path, "").Header().Get("ETag")
	wantError(t, e.do("PUT", path, `{"default":{"status":200},"checks":[{"kind":"duplicates","enforce":true}]}`, "If-Match", tag),
		http.StatusUnprocessableEntity, "invalid", "")
	ev := "d-1"
	for i := range 3 {
		e.add(t, ep.ID, fmt.Sprint(i), func(r *store.Request) { r.Delivery = &store.Delivery{Event: ev} })
	}
	e.add(t, ep.ID, "other", func(r *store.Request) { r.Delivery = &store.Delivery{Event: "d-2"} })
	list := decodeAs[struct{ Requests []summaryJSON }](t, e.do("GET", fmt.Sprintf("/endpoints/%d/requests?event=d-1", ep.ID), "")).Requests
	if len(list) != 3 || list[0].Event == nil || *list[0].Event != "d-1" || list[0].Attempt == nil || *list[0].Attempt != 3 {
		t.Fatalf("event=d-1: %+v", list)
	}
	wantError(t, e.do("GET", fmt.Sprintf("/endpoints/%d/requests?event=", ep.ID), ""), http.StatusUnprocessableEntity, "invalid", "event")
}
