package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/petroladkin/hookwitness/internal/store"
)

func TestReplayFromTheUI(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/hooks")
	var seen string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = r.Host + " " + r.RequestURI + " " + string(b)
		w.Header().Set("X-Handler", "seen")
		http.Error(w, "bad signature", http.StatusUnauthorized)
	}))
	defer target.Close()
	r := e.add(t, ep.ID, `{"a":1}`, func(r *store.Request) {
		r.Path, r.RawQuery = "/hooks", "v=2"
		r.Headers = append(r.Headers, store.Header{Name: "Content-Length", Value: "7"})
	})
	self := fmt.Sprintf("/admin/e/%d/r/%d", ep.ID, r.ID)

	// The exhibit offers Replay; ?replay opens the dialog with the request's
	// own path on localhost.
	page := e.do("GET", self, nil).Body.String()
	if !strings.Contains(page, `href="`+self+`?replay"`) || strings.Contains(page, `id="replays"`) {
		t.Fatalf("exhibit without replays:\n%s", page)
	}
	page = e.do("GET", self+"?replay", nil).Body.String()
	if !strings.Contains(page, `action="/admin/r/`+fmt.Sprint(r.ID)+`/replay"`) || !strings.Contains(page, `value="http://localhost:3000/hooks?v=2"`) {
		t.Fatalf("dialog:\n%s", page)
	}

	rec := e.do("POST", fmt.Sprintf("/admin/r/%d/replay", r.ID), url.Values{"url": {target.URL + "/webhooks"}})
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), self+"#replay-") {
		t.Fatalf("send: %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if seen != strings.TrimPrefix(target.URL, "http://")+` /webhooks {"a":1}` {
		t.Fatalf("the target saw %q", seen)
	}
	page = e.do("GET", self, nil).Body.String()
	for _, want := range []string{`id="replays"`, "Replays · 1", "401 Unauthorized", target.URL + "/webhooks", "X-Handler", "bad signature",
		"POST /webhooks HTTP/1.1"} {
		if !strings.Contains(page, want) {
			t.Errorf("the exhibit lacks %q", want)
		}
	}
	// The row is marked, and the dialog now offers the last target.
	if rows := e.do("GET", fmt.Sprintf("/admin/e/%d", ep.ID), nil).Body.String(); !strings.Contains(rows, "replayed 1") {
		t.Fatal("the register row has no replay mark")
	}
	if page := e.do("GET", self+"?replay", nil).Body.String(); !strings.Contains(page, `value="`+target.URL+`/webhooks"`) {
		t.Fatal("the dialog does not offer the last target")
	}

	// A refused target reopens the dialog with the reason; nothing is kept.
	rec = e.do("POST", fmt.Sprintf("/admin/r/%d/replay", r.ID), url.Values{"url": {"http://169.254.169.254/latest/"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Not sent:") || !strings.Contains(rec.Body.String(), "link-local") ||
		!strings.Contains(rec.Body.String(), `value="http://169.254.169.254/latest/"`) {
		t.Fatalf("refused: %d %s", rec.Code, rec.Body)
	}
	if list, _ := e.st.Replays(context.Background(), r.ID); len(list) != 1 {
		t.Fatalf("%d replays kept", len(list))
	}
	// The answer body downloads, never rendered.
	list, _ := e.st.Replays(context.Background(), r.ID)
	rec = e.do("GET", fmt.Sprintf("/admin/replays/%d/body", list[0].ID), nil)
	if rec.Header().Get("Content-Type") != "application/octet-stream" || !strings.Contains(rec.Header().Get("Content-Disposition"), "attachment") ||
		!strings.Contains(rec.Body.String(), "bad signature") {
		t.Fatalf("download: %v", rec.Header())
	}
}

// A request whose body was not kept whole offers no Replay, and its dialog
// says why.
func TestReplayNotOffered(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/hooks")
	big := e.add(t, ep.ID, "", func(r *store.Request) { r.TooLarge, r.Size, r.Body = true, 50<<20, nil })
	self := fmt.Sprintf("/admin/e/%d/r/%d", ep.ID, big.ID)
	if page := e.do("GET", self, nil).Body.String(); strings.Contains(page, "?replay") {
		t.Fatal("Replay offered for a body not kept")
	}
	if page := e.do("GET", self+"?replay", nil).Body.String(); !strings.Contains(page, "was not kept") || !strings.Contains(page, "disabled") {
		t.Fatalf("dialog for a body not kept:\n%s", page)
	}
	rec := e.do("POST", fmt.Sprintf("/admin/r/%d/replay", big.ID), url.Values{"url": {"http://127.0.0.1:1/"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("post: %d", rec.Code)
	}
}
