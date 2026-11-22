package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

type marksRead struct {
	From         int64             `json:"from"`
	KeptFrom     *int64            `json:"kept_from"`
	ThinnedUntil *int64            `json:"thinned_until"`
	Thinned      string            `json:"thinned"`
	Marks        []json.RawMessage `json:"marks"`
}

func (e env) marks(t *testing.T, path string) marksRead {
	t.Helper()
	rec := e.do("GET", path, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
	}
	var a marksRead
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	return a
}

// AC-2: every kept request of the range is on the strip, more than the
// register's page; only those of the range (with a margin).
func TestMarksCoverTheRange(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/busy")
	now := time.Now()
	e.add(t, ep.ID, `{}`, func(r *store.Request) { r.ReceivedAt = now.Add(-20 * time.Minute) }) // outside 10 min
	n := pageSize + 50
	for i := range n {
		e.add(t, ep.ID, `{"i":1}`, func(r *store.Request) {
			r.ReceivedAt = now.Add(-time.Duration(n-i) * 3 * time.Second) // the last 7.5 min
			if i == 0 {
				r.Status = 503
				r.Outcome = emulate.Outcome{Source: emulate.FromRule, Action: emulate.ActReply, Status: 503}.String()
				r.Verdict = "fail"
			}
		})
	}
	a := e.marks(t, "/admin/e/"+id(ep.ID)+"/marks?span=10m")
	if len(a.Marks) != n {
		t.Fatalf("%d marks, want %d", len(a.Marks), n)
	}
	var first []any
	json.Unmarshal(a.Marks[0], &first)
	if len(first) != 7 || first[3] != "bad" || first[4] != "fail" || first[5] == "" || first[6] != false {
		t.Fatalf("a mark: %v", first)
	}
	if a.KeptFrom != nil {
		t.Fatal("nothing was pruned")
	}
	if two := e.marks(t, "/admin/e/"+id(ep.ID)+"/marks?span=2m"); len(two.Marks) < 40 || len(two.Marks) > 46 {
		t.Fatalf("2 min: %d marks", len(two.Marks))
	}
	if six := e.marks(t, "/admin/e/"+id(ep.ID)+"/marks?span=6h"); len(six.Marks) != n+1 {
		t.Fatalf("6 h: %d marks", len(six.Marks))
	}
	if strings.Contains(e.do("GET", "/admin/e/"+id(ep.ID)+"/marks?span=6h", nil).Body.String(), `"i"`) {
		t.Fatal("bodies in the marks")
	}
}

// AC-3: the time up to the newest request retention removed is told to
// the strip: before it nothing is kept; with lanes, where only one lane
// lost requests, only the other is.
func TestMarksKeptFrom(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/small")
	now := time.Now()
	for i := range 4 {
		r := &store.Request{EndpointID: ep.ID, ReceivedAt: now.Add(-time.Duration(40-i*10) * time.Minute), Method: "POST", Path: "/small", Status: 200}
		if err := e.st.AddRequest(context.Background(), r, store.Keep{Normal: 3}, nil); err != nil {
			t.Fatal(err)
		}
	}
	a := e.marks(t, "/admin/e/"+id(ep.ID)+"/marks?span=1h")
	if a.KeptFrom == nil || *a.KeptFrom != now.Add(-40*time.Minute).UnixMilli() || a.ThinnedUntil != nil || len(a.Marks) != 3 {
		t.Fatalf("kept from %v, thinned until %v, %d marks", a.KeptFrom, a.ThinnedUntil, len(a.Marks))
	}
	// Lanes: three normal requests and an anomaly, one normal kept: before
	// the newest pruned normal one only anomalies are kept.
	lanes, _ := e.st.CreateEndpoint(context.Background(), "/lanes")
	for i := range 4 {
		r := &store.Request{EndpointID: lanes.ID, ReceivedAt: now.Add(-time.Duration(40-i*10) * time.Minute), Method: "POST", Path: "/lanes",
			Status: 200, Outcome: `{"action":"reply","status":200}`}
		if i == 0 {
			r.Status, r.Outcome = 500, `{"action":"reply","status":500}`
		}
		if err := e.st.AddRequest(context.Background(), r, store.Keep{Normal: 1, Anomalies: 5}, nil); err != nil {
			t.Fatal(err)
		}
	}
	l := e.marks(t, "/admin/e/"+id(lanes.ID)+"/marks?span=1h")
	if l.KeptFrom != nil || l.ThinnedUntil == nil || *l.ThinnedUntil != now.Add(-20*time.Minute).UnixMilli() || l.Thinned != "anomalies" || len(l.Marks) != 2 {
		t.Fatalf("lanes: kept from %v, thinned until %v (%s), %d marks", l.KeptFrom, l.ThinnedUntil, l.Thinned, len(l.Marks))
	}
	un := e.marks(t, "/admin/e/unmatched/marks?span=2m")
	if un.KeptFrom != nil || len(un.Marks) != 0 {
		t.Fatalf("unmatched: %+v", un)
	}
}

func TestMarksRefused(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/a")
	for _, span := range []string{"", "5m", "24h", "2m;"} {
		if rec := e.do("GET", "/admin/e/"+id(ep.ID)+"/marks?span="+span, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("span %q: %d", span, rec.Code)
		}
	}
	if rec := e.do("GET", "/admin/e/999/marks?span=2m", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown endpoint: %d", rec.Code)
	}
}

// AC-1: the strip's legend offers the ranges, 2 min chosen; the canvas
// knows where its marks are.
func TestStripRanges(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/a")
	page := e.do("GET", "/admin/e/"+id(ep.ID), nil).Body.String()
	for _, want := range []string{
		`data-marks="/admin/e/` + id(ep.ID) + `/marks"`,
		`<button type="button" data-span="2m" aria-pressed="true">2 min</button>`,
		`data-span="10m" aria-pressed="false"`, `data-span="1h"`, `data-span="6h"`, "hatched: not kept",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("lacks %s", want)
		}
	}
	if !strings.Contains(e.do("GET", "/admin/e/unmatched", nil).Body.String(), `data-marks="/admin/e/unmatched/marks"`) {
		t.Fatal("unmatched strip")
	}
}

// With an event id, the register's retry chips, the request page and the
// strip's groups follow the event, though the body changes on every
// attempt; without one, a changed body is a new group, as before.
func TestRetryGroupsFollowTheEvent(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/gh")
	plain, _ := e.st.CreateEndpoint(context.Background(), "/plain")
	var byEvent, byBody []*store.Request
	for i, ev := range []string{"d-1", "d-1", "d-1", "d-2"} {
		byEvent = append(byEvent, e.add(t, ep.ID, `{"attempt":`+strconv.Itoa(i+1)+`}`, func(r *store.Request) { r.Delivery = &store.Delivery{Event: ev} }))
		byBody = append(byBody, e.add(t, plain.ID, `{"attempt":`+strconv.Itoa(i+1)+`}`, func(r *store.Request) { r.Delivery = &store.Delivery{ByBody: true} }))
	}
	row := func(page string, r *store.Request) string {
		i := strings.Index(page, `id="req-`+id(r.ID)+`"`)
		if i < 0 {
			t.Fatalf("no row of request %d", r.ID)
		}
		j := strings.Index(page[i:], "</li>")
		return page[i : i+j]
	}
	page := e.do("GET", "/admin/e/"+id(ep.ID), nil).Body.String()
	for i, want := range []string{"", "retry 1", "retry 2", ""} {
		got := row(page, byEvent[i])
		if want != "" && !strings.Contains(got, `<span class="retry">`+want+`</span>`) || want == "" && strings.Contains(got, `class="retry"`) {
			t.Errorf("by event, row %d: want %q in %s", i+1, want, got)
		}
	}
	plainPage := e.do("GET", "/admin/e/"+id(plain.ID), nil).Body.String()
	for _, r := range byBody {
		if strings.Contains(row(plainPage, r), `class="retry"`) {
			t.Errorf("by body, request %d has a retry chip though its body is new", r.No)
		}
	}
	detail := e.do("GET", "/admin/e/"+id(ep.ID)+"/r/"+id(byEvent[2].ID), nil).Body.String()
	if !strings.Contains(detail, "attempt <b>3</b> of event <code>d-1</code>") {
		t.Error("the request page does not say attempt 3 of event d-1")
	}
	groups := func(endpoint int64) map[int64]string {
		g := map[int64]string{}
		for _, m := range e.marks(t, "/admin/e/"+id(endpoint)+"/marks?span=2m").Marks {
			var f []any
			json.Unmarshal(m, &f)
			g[int64(f[0].(float64))] = f[5].(string)
		}
		return g
	}
	g := groups(ep.ID)
	if a, b, c, d := g[byEvent[0].ID], g[byEvent[1].ID], g[byEvent[2].ID], g[byEvent[3].ID]; a == "" || a != b || b != c || c == d {
		t.Errorf("strip groups by event: %q %q %q %q", a, b, c, d)
	}
	g = groups(plain.ID)
	if a, b := g[byBody[0].ID], g[byBody[1].ID]; a == b {
		t.Errorf("strip groups by body: two different bodies in one group %q", a)
	}
}
