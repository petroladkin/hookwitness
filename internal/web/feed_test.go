package web

import (
	"bufio"
	"context"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/store"
)

type sseEvent struct{ id, kind, data string }

// sseEvents parses a feed into events on a channel.
func sseEvents(body io.Reader) <-chan sseEvent {
	out := make(chan sseEvent, 64)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(body)
		sc.Buffer(nil, 1<<20)
		var ev sseEvent
		for sc.Scan() {
			l := sc.Text()
			switch {
			case strings.HasPrefix(l, "id: "):
				ev.id = l[4:]
			case strings.HasPrefix(l, "event: "):
				ev.kind = l[7:]
			case strings.HasPrefix(l, "data: "):
				ev.data += l[6:] + "\n"
			case l == "":
				if ev.kind != "" {
					out <- ev
				}
				ev = sseEvent{}
			}
		}
	}()
	return out
}

// until reads events until one of kind arrives (returning the ones before
// it too), or fails after a timeout.
func until(t *testing.T, evs <-chan sseEvent, kind string) []sseEvent {
	t.Helper()
	var got []sseEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-evs:
			if !ok {
				t.Fatalf("feed ended before a %q event; got %+v", kind, got)
			}
			got = append(got, ev)
			if ev.kind == kind {
				return got
			}
		case <-timeout:
			t.Fatalf("no %q event; got %+v", kind, got)
		}
	}
}

func updatesOf(evs []sseEvent) (ids []string) {
	re := regexp.MustCompile(`id="req-(\d+)"`)
	for _, ev := range evs {
		if ev.kind == "update" {
			if m := re.FindStringSubmatch(ev.data); m != nil {
				ids = append(ids, m[1])
			}
		}
	}
	return ids
}

// TestLiveFeedResumesChanges: a change made between the page's render and
// its feed's first connect, and one made while the feed was disconnected,
// both reach the page after it (re)connects; nothing is sent twice.
func TestLiveFeedResumesChanges(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, _ := e.st.CreateEndpoint(ctx, "/resume")
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	a := e.add(t, ep.ID, "{}", func(r *store.Request) {
		r.Status, r.Outcome = 200, `{"source":"default","action":"reply","status":200}`
	})
	b := e.add(t, ep.ID, "{}", func(r *store.Request) {
		r.Status, r.Outcome = 200, `{"source":"default","action":"reply","status":200}`
	})

	page := e.do("GET", "/admin/e/"+id(ep.ID), nil).Body.String()
	m := regexp.MustCompile(`sse-connect="([^"]+)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no feed on the page")
	}
	feedURL := html.UnescapeString(m[1])

	// Between the render and the first connect, a's outcome changes.
	e.st.UpdateOutcome(ctx, a.ID, 500, `{"source":"default","action":"error","status":500,"error":"boom"}`)
	resp, err := http.Get(srv.URL + feedURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() // before the server's Close, which waits for open feeds
	got := until(t, sseEvents(resp.Body), "counts")
	if u := updatesOf(got); strings.Join(u, ",") != id(a.ID) {
		t.Fatalf("first connect sent updates %v, want only %d", u, a.ID)
	}
	resume := got[len(got)-1].id
	if !strings.HasPrefix(resume, id(b.ID)+".") {
		t.Fatalf("the pass ends with id %q, want %d.<seq>", resume, b.ID)
	}
	resp.Body.Close()

	// While disconnected, b's outcome changes; the browser reconnects with
	// the last id it got.
	e.st.UpdateOutcome(ctx, b.ID, 500, `{"source":"default","action":"error","status":500,"error":"boom"}`)
	req, _ := http.NewRequest("GET", srv.URL+feedURL, nil)
	req.Header.Set("Last-Event-ID", resume)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got = until(t, sseEvents(resp.Body), "counts")
	if u := updatesOf(got); strings.Join(u, ",") != id(b.ID) {
		t.Fatalf("reconnect sent updates %v, want only %d (a was sent already)", u, b.ID)
	}
}

// TestLiveFeedOldPageReconnects: a page of the previous version sends a
// plain request id as Last-Event-ID; the feed resumes after it and sends
// changes from then on.
func TestLiveFeedOldPageReconnects(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, _ := e.st.CreateEndpoint(ctx, "/old")
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	a := e.add(t, ep.ID, "{}", nil)
	b := e.add(t, ep.ID, "{}", nil)
	req, _ := http.NewRequest("GET", srv.URL+"/admin/e/"+id(ep.ID)+"/events?after=0", nil)
	req.Header.Set("Last-Event-ID", id(a.ID))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	evs := sseEvents(resp.Body)
	got := until(t, evs, "counts")
	if len(got) != 2 || got[0].kind != "request" || !strings.HasPrefix(got[0].id, id(b.ID)+".") {
		t.Fatalf("events %+v, want b then counts", got)
	}
	e.st.UpdateOutcome(ctx, a.ID, 500, `{"source":"default","action":"error","status":500,"error":"boom"}`)
	e.lv.Publish(ep.ID)
	if u := updatesOf(until(t, evs, "counts")); strings.Join(u, ",") != id(a.ID) {
		t.Fatalf("updates %v, want %d", u, a.ID)
	}
}

func TestResumeAt(t *testing.T) {
	for _, c := range []struct {
		in        string
		last, seq int64
		ok        bool
	}{
		{"12.7", 12, 7, true},
		{"12", 12, -1, true},
		{"0.0", 0, 0, true},
		{"", 0, 0, false},
		{"x.1", 0, 0, false},
		{"1.x", 0, 0, false},
		{"-1.2", 0, 0, false},
		{"1.-2", 0, 0, false},
	} {
		last, seq, ok := resumeAt(c.in)
		if last != c.last || seq != c.seq || ok != c.ok {
			t.Errorf("%q: %d %d %v, want %d %d %v", c.in, last, seq, ok, c.last, c.seq, c.ok)
		}
	}
}
