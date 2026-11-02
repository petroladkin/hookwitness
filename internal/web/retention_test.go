package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/petroladkin/hookwitness/internal/store"
)

// TestRetentionDialog: the dialog shows the limits, a save that would
// delete names how many first, the confirmed save prunes at once, and the
// defaults come back with one button.
func TestRetentionDialog(t *testing.T) {
	e := newEnvWith(t, Options{Prefix: "/admin", MaxBody: 10 << 20, Retention: store.Keep{Normal: 1000, Anomalies: 1000}, Hosts: []string{"example.com"}})
	ep, _ := e.st.CreateEndpoint(context.Background(), "/keep")
	for i := range 8 {
		e.add(t, ep.ID, fmt.Sprint(i), func(r *store.Request) { r.Outcome = `{"action":"reply","status":200}` })
	}
	base := "/admin/e/" + id(ep.ID)
	page := e.do("GET", base, nil).Body.String()
	if !strings.Contains(page, "keeps 1000 newest") || !strings.Contains(page, "1000 anomalies") || !strings.Contains(page, `href="`+base+`?retention"`) {
		i := strings.Index(page, "class=\"keeps\"")
		t.Fatalf("header line or button missing: %q", page[max(0, i-200):min(len(page), i+600)])
	}
	dlg := e.do("GET", base+"?retention", nil).Body.String()
	if !strings.Contains(dlg, `action="`+base+`/retention"`) || !strings.Contains(dlg, `placeholder="1000"`) {
		t.Fatalf("dialog: %s", dlg)
	}
	// Lowering: the first post previews, nothing changes.
	rec := e.do("POST", base+"/retention", url.Values{"normal_count": {"3"}, "normal_age": {"7d"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Saving prunes 5 requests now") ||
		!strings.Contains(rec.Body.String(), `name="confirm" value="5"`) {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	if got, _ := e.st.Endpoint(context.Background(), ep.ID); got.Count != 8 || !got.Kept.Policy.IsZero() {
		t.Fatalf("the preview changed something: %+v", got.Kept)
	}
	rec = e.do("POST", base+"/retention", url.Values{"normal_count": {"3"}, "normal_age": {"7d"}, "confirm": {"5"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	got, _ := e.st.Endpoint(context.Background(), ep.ID)
	if got.Count != 3 || got.Kept.Policy.Normal.Count != 3 || got.Kept.Pruned != 5 {
		t.Fatalf("after the save: %d kept, %+v", got.Count, got.Kept)
	}
	if page := e.do("GET", base, nil).Body.String(); !strings.Contains(page, "keeps 3 newest") || !strings.Contains(page, "for 7d") ||
		!strings.Contains(page, ">5</span> pruned") {
		t.Fatalf("header after the save: %s", page[:min(len(page), 4000)])
	}
	// Invalid fields: 422, the field marked, nothing saved.
	rec = e.do("POST", base+"/retention", url.Values{"normal_count": {"0"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Not saved: Normal requests") {
		t.Fatalf("invalid count: %d", rec.Code)
	}
	rec = e.do("POST", base+"/retention", url.Values{"anomaly_age": {"5y"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid age: %d", rec.Code)
	}
	// Back to the defaults.
	if rec := e.do("POST", base+"/retention", url.Values{"op": {"defaults"}, "normal_count": {"3"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("defaults: %d", rec.Code)
	}
	if got, _ := e.st.Endpoint(context.Background(), ep.ID); !got.Kept.Policy.IsZero() {
		t.Fatalf("defaults left %+v", got.Kept.Policy)
	}
	// Unmatched: one lane, no anomaly fields.
	if dlg := e.do("GET", "/admin/e/unmatched?retention", nil).Body.String(); strings.Contains(dlg, "anomaly_count") {
		t.Fatal("the Unmatched dialog offers anomalies")
	}
}

// TestPinFromThePage: Pin and Unpin on a request, the marker in the row,
// and a clear that names the pinned requests it removes.
func TestPinFromThePage(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/pin")
	r := e.add(t, ep.ID, "x", nil)
	self := "/admin/e/" + id(ep.ID) + "/r/" + id(r.ID)
	if page := e.do("GET", self, nil).Body.String(); !strings.Contains(page, `action="/admin/r/`+id(r.ID)+`/pin"`) || !strings.Contains(page, ">Pin</button>") {
		t.Fatal("no Pin on the request page")
	}
	rec := e.do("POST", "/admin/r/"+id(r.ID)+"/pin", url.Values{"pinned": {"true"}, "next": {self}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != self {
		t.Fatalf("pin: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	page := e.do("GET", self, nil).Body.String()
	if !strings.Contains(page, ">Unpin</button>") || !strings.Contains(page, `data-lane="pinned"`) || !strings.Contains(page, "tag pinned") {
		t.Fatal("pinned request not shown pinned")
	}
	if !strings.Contains(page, "the 1 pinned one too?") {
		t.Fatal("the clear does not name the pinned request")
	}
	e.do("POST", "/admin/r/"+id(r.ID)+"/pin", url.Values{"pinned": {"false"}})
	if got, _ := e.st.Request(context.Background(), r.ID); got.Pinned {
		t.Fatal("not unpinned")
	}
	if rec := e.do("POST", "/admin/r/999/pin", url.Values{"pinned": {"true"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("a missing request: %d", rec.Code)
	}
}

func TestThousands(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1 000", 41208: "41 208", 1234567: "1 234 567"} {
		if got := thousands(n); got != want {
			t.Errorf("thousands(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestImportReviewNamesThePrune: replacing an endpoint with a file whose
// retention is lower says, in the review, how many requests go.
func TestImportReviewNamesThePrune(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/imp")
	for i := range 5 {
		e.add(t, ep.ID, fmt.Sprint(i), nil)
	}
	file := `{"hookwitness": 2, "endpoints": [{"path": "/imp", "default": {"status": 200}, "retention": {"normal": {"count": 2}}}]}`
	rec := e.do("POST", "/admin/import", url.Values{"op": {"review"}, "text": {file}})
	if !strings.Contains(rec.Body.String(), "prunes 3 of its requests at once") {
		t.Fatalf("review: %d %s", rec.Code, rec.Body)
	}
	rec = e.do("POST", "/admin/import", url.Values{"op": {"import"}, "text": {file}, "path_0": {"/imp"}, "replace_0": {"on"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("import: %d %s", rec.Code, rec.Body)
	}
	if got, _ := e.st.Endpoint(context.Background(), ep.ID); got.Count != 2 || got.Kept.Policy.Normal.Count != 2 {
		t.Fatalf("after the import: %d kept, %+v", got.Count, got.Kept.Policy)
	}
}
