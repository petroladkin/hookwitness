package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/store"
)

// TestStorage: the database's size, its limit (null without one), pinned
// bytes and the volume.
func TestStorage(t *testing.T) {
	e := newEnv(t)
	got := decodeAs[storageJSON](t, e.do("GET", "/storage", ""))
	if got.UsedBytes <= 0 || got.LimitBytes != nil || got.VolumeFree == nil || *got.VolumeTotal <= 0 || got.Warnings == nil {
		t.Fatalf("without a limit: %+v", got)
	}
	e.h.o.MaxDBSize = got.UsedBytes // at the limit: a warning
	got = decodeAs[storageJSON](t, e.do("GET", "/storage", ""))
	if got.LimitBytes == nil || *got.LimitBytes != e.h.o.MaxDBSize || len(got.Warnings) == 0 || !strings.Contains(got.Warnings[0], "% of its size limit") {
		t.Fatalf("at the limit: %+v", got)
	}
}

// TestDroppedBody is AC-B3 for the API: a request whose body was dropped
// says so, keeps its size, digest and verdicts; its raw body and images
// answer 404 not_kept; a replay is refused; a re-check keeps its verdicts
// with a note.
func TestDroppedBody(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, "/hooks")
	r := e.add(t, ep.ID, `{"a":1}`, func(r *store.Request) {
		r.Verdict = "pass"
		r.Verdicts = `[{"check":1,"label":"Basic credentials","result":"pass","reason":"user u","at":"2026-11-03T10:00:00Z"}]`
	})
	if err := e.st.DropBodies(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	got := decodeAs[requestJSON](t, e.do("GET", fmt.Sprintf("/requests/%d", r.ID), ""))
	if !got.BodyDropped || got.BodyKept || got.BodySize != 7 || got.BodyText != nil || got.BodyBase64 != nil || got.Verdict != "pass" {
		t.Fatalf("request %+v", got.summaryJSON)
	}
	if l := e.list(t, "?include=body"); !l.Requests[0].BodyDropped || l.Requests[0].BodyKept {
		t.Fatalf("list %+v", l.Requests[0])
	}
	for _, path := range []string{"/requests/%d/body", "/requests/%d/image?path="} {
		got := wantError(t, e.do("GET", fmt.Sprintf(path, r.ID), ""), http.StatusNotFound, "not_kept", "")
		if !strings.Contains(got.Error.Message, "dropped to stay under the database size limit") {
			t.Fatalf("%s: %q", path, got.Error.Message)
		}
	}
	rp := wantError(t, e.do("POST", fmt.Sprintf("/requests/%d/replays", r.ID), `{"url":"http://127.0.0.1:9/x"}`), http.StatusConflict, "body_not_kept", "")
	if !strings.Contains(rp.Error.Message, "dropped to stay under the database size limit") {
		t.Fatalf("replay: %q", rp.Error.Message)
	}
	e.do("PUT", fmt.Sprintf("/endpoints/%d/secrets/PW", ep.ID), `{"kind":"token","value":"test-pw"}`)
	_, tag := e.behaviour(t, ep.ID)
	e.putBehaviour(ep.ID, tag, `{"checks":[{"kind":"basic","secret":"PW","user":"u"}]}`)
	e.do("POST", fmt.Sprintf("/endpoints/%d/recheck", ep.ID), "{}")
	got = decodeAs[requestJSON](t, e.do("GET", fmt.Sprintf("/requests/%d", r.ID), ""))
	if got.Verdict != "pass" || len(got.Verdicts) != 1 || got.Verdicts[0].Result != "pass" || !strings.Contains(got.Verdicts[0].Note, "body dropped") {
		t.Fatalf("after a re-check: %s %+v", got.Verdict, got.Verdicts)
	}
}

// TestDroppedAnswers: the answer bodies of a dropped request's replays and
// forward go with it and say so; a pinned request's stay.
func TestDroppedAnswers(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	ep := e.endpoint(t, "/hooks")
	answers := func(id int64) int64 {
		const out = `{"source":"forward","action":"forward","status":201,"forward":{"target":"https://receiver.test/in","status":201,"ms":3}}`
		if err := e.st.FinishForward(ctx, &store.Forward{RequestID: id, CreatedAt: time.Now(), URL: "https://receiver.test/in",
			Options: `{"host":"target"}`, Line: "POST /in HTTP/1.1", Proto: "HTTP/1.1", Status: 201, Reason: "Created",
			Body: []byte("made"), Size: 4}, 201, out); err != nil {
			t.Fatal(err)
		}
		rp := &store.Replay{RequestID: id, CreatedAt: time.Now(), URL: "http://127.0.0.1:9/x", Options: `{"host":"target"}`,
			Line: "POST /x HTTP/1.1", Proto: "HTTP/1.1", Status: 200, Reason: "OK", Body: []byte("fine"), Size: 4}
		if err := e.st.AddReplay(ctx, rp); err != nil {
			t.Fatal(err)
		}
		return rp.ID
	}
	gone := e.add(t, ep.ID, `{"a":1}`, nil)
	goneReplay := answers(gone.ID)
	pinned := e.add(t, ep.ID, `{"a":2}`, nil)
	pinnedReplay := answers(pinned.ID)
	if err := e.st.SetPinned(ctx, pinned.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := e.st.DropBodies(ctx, gone.ID, pinned.ID); err != nil {
		t.Fatal(err)
	}

	fw := decodeAs[forwardJSON](t, e.do("GET", fmt.Sprintf("/requests/%d/forward", gone.ID), ""))
	if !fw.BodyDropped || fw.BodyText != nil || fw.BodyBase64 != nil || fw.BodySize != 4 || fw.Status != 201 {
		t.Fatalf("forward %+v", fw)
	}
	rp := decodeAs[replayJSON](t, e.do("GET", fmt.Sprintf("/replays/%d", goneReplay), ""))
	if !rp.BodyDropped || rp.BodyText != nil || rp.BodyBase64 != nil || rp.BodySize != 4 || rp.Status != 200 {
		t.Fatalf("replay %+v", rp)
	}
	for _, path := range []string{fmt.Sprintf("/requests/%d/forward/body", gone.ID), fmt.Sprintf("/replays/%d/body", goneReplay)} {
		got := wantError(t, e.do("GET", path, ""), http.StatusNotFound, "not_kept", "")
		if !strings.Contains(got.Error.Message, "answer body was dropped") {
			t.Fatalf("%s: %q", path, got.Error.Message)
		}
	}

	if fw := decodeAs[forwardJSON](t, e.do("GET", fmt.Sprintf("/requests/%d/forward", pinned.ID), "")); fw.BodyDropped || fw.BodyText == nil || *fw.BodyText != "made" {
		t.Fatalf("a pinned request's forward %+v", fw)
	}
	if rec := e.do("GET", fmt.Sprintf("/replays/%d/body", pinnedReplay), ""); rec.Code != 200 || rec.Body.String() != "fine" {
		t.Fatalf("a pinned request's replay: %d %q", rec.Code, rec.Body)
	}
}
