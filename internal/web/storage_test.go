package web

import (
	"context"
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
	if !strings.Contains(page, "Database ") || !strings.Contains(page, "no size limit (-max-db-size)") || !strings.Contains(page, "the volume has ") ||
		strings.Contains(page, "storage-warn") {
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
	if err := e.st.DropBodies(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	ex := e.do("GET", "/admin/e/"+id(ep.ID)+"/r/"+id(r.ID), nil).Body.String()
	if !strings.Contains(ex, "Body not kept: dropped to stay under the database size limit") || strings.Contains(ex, "Download body") ||
		strings.Contains(ex, "?replay") {
		t.Fatal("the request page of a dropped body")
	}
	if rec := e.do("GET", "/admin/r/"+id(r.ID)+"/body", nil); rec.Code != 404 || !strings.Contains(rec.Body.String(), "dropped") {
		t.Fatalf("raw body: %d %q", rec.Code, rec.Body)
	}
}
