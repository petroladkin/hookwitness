package web

import (
	"context"
	"html"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/store"
)

// TestStorageShown is AC-B5 and AC-B6 for the UI: the endpoints page names
// the database's size (and its limit, pinned bytes, the volume's space);
// at the limit every page warns.
func TestStorageShown(t *testing.T) {
	e := newEnv(t)
	page := e.do("GET", "/admin/", nil).Body.String()
	// No warning of a limit (the volume's own warning depends on the
	// machine; TestStorageWarnings checks it).
	if !strings.Contains(page, "Database ") || !strings.Contains(page, "no size limit (-max-db-size)") || !strings.Contains(page, "the volume has ") ||
		strings.Contains(page, "of its size limit") || strings.Contains(page, "pinned requests alone") || strings.Contains(page, "left is pinned") {
		t.Fatal("the endpoints page without a limit")
	}
	used, _ := e.st.Usage(context.Background())
	e = newEnvWith(t, Options{Prefix: "/admin", MaxBody: 10 << 20, Retention: store.Keep{Normal: 1000}, MaxDBSize: used,
		Hosts: []string{"hooks.example.test", "example.com"}})
	ep, _ := e.st.CreateEndpoint(context.Background(), "/a")
	page = e.do("GET", "/admin/", nil).Body.String()
	if !strings.Contains(page, " of "+store.Size(used)) || !strings.Contains(page, "<b>Storage.</b> the database is at") {
		t.Fatal("the endpoints page at the limit")
	}
	if reg := e.do("GET", "/admin/e/"+id(ep.ID), nil).Body.String(); !strings.Contains(reg, "<b>Storage.</b> the database is at") {
		t.Fatal("no warning on a register page")
	}
}

// TestDroppedBodyPage is AC-B3 for the UI: the request page says why the
// body is gone; no download, no replay; the raw body answers 404.
func TestDroppedBodyPage(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, _ := e.st.CreateEndpoint(ctx, "/a")
	r := &store.Request{EndpointID: ep.ID, ReceivedAt: time.Now(), Method: "POST", Path: "/a", Proto: "HTTP/1.1",
		Headers: []store.Header{{Name: "Content-Type", Value: "application/json"}}, Body: []byte(`{"a":1}`), Size: 7, Status: 200,
		Outcome: `{"source":"default","action":"reply","status":200}`}
	if err := e.st.AddRequest(ctx, r, store.Keep{Normal: 100}, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.st.FinishForward(ctx, &store.Forward{RequestID: r.ID, CreatedAt: time.Now(), URL: "https://receiver.test/in",
		Options: `{"host":"target"}`, Line: "POST /in HTTP/1.1", Proto: "HTTP/1.1", Status: 201, Reason: "Created", Body: []byte("made"), Size: 4},
		201, `{"source":"forward","action":"forward","status":201,"forward":{"target":"https://receiver.test/in","status":201,"ms":3}}`); err != nil {
		t.Fatal(err)
	}
	rp := &store.Replay{RequestID: r.ID, CreatedAt: time.Now(), URL: "http://127.0.0.1:9/x", Options: `{"host":"target"}`,
		Line: "POST /x HTTP/1.1", Proto: "HTTP/1.1", Status: 200, Reason: "OK", Body: []byte("fine"), Size: 4}
	if err := e.st.AddReplay(ctx, rp); err != nil {
		t.Fatal(err)
	}
	if err := e.st.DropBodies(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	ex := e.do("GET", "/admin/e/"+id(ep.ID)+"/r/"+id(r.ID), nil).Body.String()
	if n := strings.Count(ex, "Answer body not kept: dropped with the request's body"); n != 2 || !strings.Contains(ex, "It was 4 B.") ||
		strings.Contains(ex, "Download the answer body") || strings.Contains(ex, "Download the receiver's answer body") {
		t.Fatalf("%d answers said dropped", n)
	}
	for _, path := range []string{"/admin/r/" + id(r.ID) + "/forward/body", "/admin/replays/" + id(rp.ID) + "/body"} {
		if rec := e.do("GET", path, nil); rec.Code != 404 || !strings.Contains(rec.Body.String(), "answer body was dropped") {
			t.Fatalf("%s: %d %q", path, rec.Code, rec.Body)
		}
	}
	if !strings.Contains(ex, "Body not kept: dropped to stay under the database size limit") || strings.Contains(ex, "Download body") ||
		strings.Contains(ex, "?replay") {
		t.Fatal("the request page of a dropped body")
	}
	if rec := e.do("GET", "/admin/r/"+id(r.ID)+"/body", nil); rec.Code != 404 || !strings.Contains(rec.Body.String(), "dropped") {
		t.Fatalf("raw body: %d %q", rec.Code, rec.Body)
	}
}

// A draft by a header's value counts and takes only requests whose bodies
// are kept.
func TestDraftSkipsDroppedBodies(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, _ := e.st.CreateEndpoint(ctx, "/shop/events")
	base := "/admin/e/" + id(ep.ID)
	e.edit(ep.ID, url.Values{"op": {"schema-save"}, "i": {"new"}, "name": {"OrderPaid"}, "text": {orderPaid}})
	e.edit(ep.ID, url.Values{"op": {"check-save"}, "i": {"new"}, "kind": {"schema"}, "select_from": {"header"}, "select_key": {"X-Event"},
		"select_map": {"order.paid = OrderPaid"}})
	var ids []int64
	for i := range 3 {
		r := e.add(t, ep.ID, `{"order_id":`+strconv.Itoa(i)+`}`, func(r *store.Request) {
			r.Headers = append(r.Headers, store.Header{Name: "X-Event", Value: "order.shipped"})
		})
		ids = append(ids, r.ID)
	}
	if err := e.st.DropBodies(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	p := html.UnescapeString(e.do("GET", base+"/behaviour?tab=schemas&edit=schema-draft&by=value&value=order.shipped", nil).Body.String())
	if !strings.Contains(p, "Drafted from the 2 requests with header X-Event = order.shipped.") || strings.Contains(p, "was dropped") {
		t.Fatalf("draft by value:\n%s", between(p, "<dialog", "</dialog>"))
	}
}
