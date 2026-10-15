package web

import (
	"bufio"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/live"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
)

type env struct {
	h  *Handler
	st *store.Store
	lv *live.Broker
}

func newEnv(t *testing.T) env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	lv := live.New()
	keys, err := secret.Load("", filepath.Join(t.TempDir(), "hw.db.key"))
	if err != nil {
		t.Fatal(err)
	}
	return env{New(st, lv, slog.New(slog.NewTextHandler(io.Discard, nil)),
		Options{Prefix: "/admin", MaxBody: 10 << 20, Retention: 1000, Hosts: []string{"hooks.example.test", "example.com"}, Keys: keys}), st, lv}
}

func (e env) do(method, target string, form url.Values, hdr ...string) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, target, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	return rec
}

func (e env) add(t *testing.T, endpointID int64, body string, mod func(*store.Request)) *store.Request {
	t.Helper()
	r := &store.Request{EndpointID: endpointID, ReceivedAt: time.Now(), Method: "POST", Path: "/x", Proto: "HTTP/1.1",
		RemoteAddr: "203.0.113.9:4000", Headers: []store.Header{{Name: "Content-Type", Value: "application/json"}},
		Body: []byte(body), Size: int64(len(body)), Status: 200}
	if mod != nil {
		mod(r)
	}
	if err := e.st.AddRequest(context.Background(), r, 1000, nil); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCreateEndpoint(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/admin/endpoints", url.Values{"path": {"/stripe/webhooks"}})
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/admin/e/") {
		t.Fatalf("create: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	refused := map[string]string{
		"/stripe/webhooks":             "already an endpoint",
		"stripe":                       `must start with &#34;/&#34;`,
		"/a//b":                        "empty segment",
		"/a b":                         "not allowed",
		"/a/../b":                      "not allowed",
		"/healthz":                     "reserved",
		"/admin":                       "reserved",
		"/admin/x":                     "reserved",
		"/" + strings.Repeat("a", 600): "longer than",
	}
	for p, msg := range refused {
		rec := e.do("POST", "/admin/endpoints", url.Values{"path": {p}})
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), msg) {
			t.Errorf("%q: %d, want 422 with %q", p, rec.Code, msg)
		}
	}
	for _, p := range []string{"/", "/a/", "/a.b/c~d_e-f", "/administrator"} {
		if rec := e.do("POST", "/admin/endpoints", url.Values{"path": {p}}); rec.Code != http.StatusSeeOther {
			t.Errorf("%q refused: %d", p, rec.Code)
		}
	}
	// Empty: a random path.
	e.do("POST", "/admin/endpoints", url.Values{"path": {""}})
	eps, _ := e.st.Endpoints(context.Background())
	random := regexp.MustCompile(`^/[a-z2-9]{12}$`)
	found := false
	for _, ep := range eps {
		found = found || random.MatchString(ep.Path)
	}
	if !found {
		t.Fatalf("no random endpoint among %+v", eps)
	}
}

func TestCrossSiteFormRefused(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/admin/endpoints", url.Values{"path": {"/x"}}, "Sec-Fetch-Site", "cross-site")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d, want 403", rec.Code)
	}
	if eps, _ := e.st.Endpoints(context.Background()); len(eps) != 0 {
		t.Fatal("cross-site POST created an endpoint")
	}
	if rec := e.do("POST", "/admin/endpoints", url.Values{"path": {"/x"}}, "Sec-Fetch-Site", "same-origin"); rec.Code != http.StatusSeeOther {
		t.Fatalf("same-origin POST: %d", rec.Code)
	}
}

func TestEndpointsPageListsCountsAndLast(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/hooks/a")
	e.st.CreateEndpoint(context.Background(), "/hooks/b")
	e.add(t, ep.ID, "{}", nil)
	e.add(t, ep.ID, "{}", func(r *store.Request) { r.ReceivedAt = time.Date(2026, 10, 11, 9, 8, 7, 654_000_000, time.UTC) })
	body := e.do("GET", "/admin/", nil).Body.String()
	for _, want := range []string{"/hooks/a", "/hooks/b", ">2<", "never", "Unmatched requests"} {
		if !strings.Contains(body, want) {
			t.Errorf("endpoints page lacks %q", want)
		}
	}
}

func TestClearAndDelete(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, _ := e.st.CreateEndpoint(ctx, "/c")
	e.add(t, ep.ID, "{}", nil)
	e.add(t, store.Unmatched, "{}", nil)

	rec := e.do("POST", "/admin/e/"+id(ep.ID)+"/clear", url.Values{"next": {"/admin/"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/" {
		t.Fatalf("clear: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if got, _ := e.st.Endpoint(ctx, ep.ID); got.Count != 0 {
		t.Fatal("clear left requests")
	}
	// "next" must stay inside the UI.
	rec = e.do("POST", "/admin/e/unmatched/clear", url.Values{"next": {"https://evil.example/"}})
	if loc := rec.Header().Get("Location"); loc != "/admin/e/unmatched" {
		t.Fatalf("clear Unmatched redirected to %q", loc)
	}
	if u, _ := e.st.Endpoint(ctx, store.Unmatched); u.Count != 0 {
		t.Fatal("Unmatched not cleared")
	}

	if rec := e.do("POST", "/admin/e/unmatched/delete", url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("deleting Unmatched: %d, want 404", rec.Code)
	}
	if rec := e.do("POST", "/admin/e/"+id(ep.ID)+"/delete", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", rec.Code)
	}
	if _, err := e.st.Endpoint(ctx, ep.ID); err == nil {
		t.Fatal("endpoint still there")
	}
	if rec := e.do("POST", "/admin/e/"+id(ep.ID)+"/delete", url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d", rec.Code)
	}
	if rec := e.do("GET", "/admin/e/"+id(ep.ID), nil); rec.Code != http.StatusNotFound {
		t.Fatalf("page of a deleted endpoint: %d", rec.Code)
	}
}

func TestRegisterAndExhibit(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/r")
	first := e.add(t, ep.ID, `{"b":1,"a":[true,null,"<script>"]}`, func(r *store.Request) { r.RawQuery = "x=1&x=%202" })
	retry := e.add(t, ep.ID, `{"b":1,"a":[true,null,"<script>"]}`, nil)
	big := e.add(t, ep.ID, "", func(r *store.Request) { r.TooLarge, r.Size, r.Status = true, 20<<20, 413 })

	page := e.do("GET", "/admin/e/"+id(ep.ID), nil)
	body := page.Body.String()
	if page.Code != 200 {
		t.Fatalf("register page %d", page.Code)
	}
	if !strings.Contains(body, `sse-connect="/admin/e/`+id(ep.ID)+`/events?after=`+id(big.ID)+`"`) {
		t.Error("live feed does not start after the newest row")
	}
	// Newest first.
	if strings.Index(body, `id="req-`+id(big.ID)) > strings.Index(body, `id="req-`+id(first.ID)) {
		t.Error("register is not newest first")
	}
	if !strings.Contains(body, `data-keep="1000"`) {
		t.Error("the page does not know the retention limit")
	}
	if !strings.Contains(body, "retry 1") || !strings.Contains(body, "Refused: body over the limit") {
		t.Error("retry or refusal not shown in the register")
	}
	for _, hdr := range []string{"Content-Security-Policy", "X-Content-Type-Options"} {
		if page.Header().Get(hdr) == "" {
			t.Errorf("missing %s", hdr)
		}
	}

	ex := e.do("GET", "/admin/e/"+id(ep.ID)+"/r/"+id(first.ID), nil).Body.String()
	for _, want := range []string{
		`<span class="k">&#34;b&#34;</span>`,            // key order kept: b before a
		`&#34;<script&#34;`[:0] + `&lt;script&gt;`,      // escaped, never markup
		`<th>x</th><td>1</td>`, `<th>x</th><td> 2</td>`, // query decoded, order kept
		"200 sent", "Download body", "Raw view",
	} {
		if !strings.Contains(ex, want) {
			t.Errorf("exhibit lacks %q", want)
		}
	}
	if strings.Contains(ex, "<script>\"") || strings.Contains(ex, `"<script>`) {
		t.Error("body rendered as markup")
	}
	if strings.Index(ex, `&#34;b&#34;`) > strings.Index(ex, `&#34;a&#34;`) {
		t.Error("JSON keys reordered")
	}

	frag := e.do("GET", "/admin/e/"+id(ep.ID)+"/r/"+id(retry.ID), nil, "HX-Request", "true").Body.String()
	if strings.Contains(frag, "<html") || !strings.Contains(frag, "delivery <b>2</b> of this body") {
		t.Errorf("HTMX request should get the exhibit alone, with the delivery count")
	}
	restore := e.do("GET", "/admin/e/"+id(ep.ID)+"/r/"+id(retry.ID), nil, "HX-Request", "true", "HX-History-Restore-Request", "true").Body.String()
	if !strings.Contains(restore, "<html") {
		t.Error("history restore must get the full page")
	}

	tooBig := e.do("GET", "/admin/e/"+id(ep.ID)+"/r/"+id(big.ID), nil).Body.String()
	for _, want := range []string{"413 sent", "20.0 MiB", "10.0 MiB", "-max-body"} {
		if !strings.Contains(tooBig, want) {
			t.Errorf("refused exhibit lacks %q", want)
		}
	}
	if strings.Contains(tooBig, "Download body") {
		t.Error("nothing to download for a refused body")
	}

	// A request of another register is not shown under this one.
	other, _ := e.st.CreateEndpoint(context.Background(), "/other")
	if rec := e.do("GET", "/admin/e/"+id(other.ID)+"/r/"+id(first.ID), nil); rec.Code != 404 {
		t.Errorf("request under the wrong endpoint: %d", rec.Code)
	}
}

func TestBinaryAndDownload(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/bin")
	raw := "\x00\x01\x02PNG\xff\xfe<script>"
	r := e.add(t, ep.ID, raw, nil)
	ex := e.do("GET", "/admin/e/"+id(ep.ID)+"/r/"+id(r.ID), nil).Body.String()
	if !strings.Contains(ex, "00000000  00 01 02 50 4e 47 ff fe") || !strings.Contains(ex, "Binary, 16 B") {
		t.Errorf("binary body not shown as hex:\n%s", ex)
	}
	dl := e.do("GET", "/admin/r/"+id(r.ID)+"/body", nil)
	if dl.Body.String() != raw || dl.Header().Get("Content-Type") != "application/octet-stream" ||
		!strings.HasPrefix(dl.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("download: %q %v", dl.Body, dl.Header())
	}
	if rec := e.do("GET", "/admin/r/999/body", nil); rec.Code != 404 {
		t.Fatalf("missing body: %d", rec.Code)
	}
}

func TestOlderRows(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/many")
	for range pageSize + 5 {
		e.add(t, ep.ID, "{}", nil)
	}
	page := e.do("GET", "/admin/e/"+id(ep.ID), nil).Body.String()
	if n := strings.Count(page, `class="row`); n != pageSize {
		t.Fatalf("first page has %d rows, want %d", n, pageSize)
	}
	m := regexp.MustCompile(`rows\?before=(\d+)`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no Show older control")
	}
	older := e.do("GET", "/admin/e/"+id(ep.ID)+"/rows?before="+m[1], nil).Body.String()
	if n := strings.Count(older, `class="row`); n != 5 || strings.Contains(older, "Show older") {
		t.Fatalf("older page: %d rows (want 5), more control %v", n, strings.Contains(older, "Show older"))
	}
}

// readEvents reads SSE events until n request events arrived or the timeout.
func readEvents(t *testing.T, body io.Reader, n int) []string {
	t.Helper()
	ids := make(chan string)
	go func() {
		sc := bufio.NewScanner(body)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "id: "); ok {
				ids <- v
			}
		}
		close(ids)
	}()
	var got []string
	timeout := time.After(5 * time.Second)
	for len(got) < n {
		select {
		case v, ok := <-ids:
			if !ok {
				return got
			}
			got = append(got, v)
		case <-timeout:
			t.Fatalf("got events %v, want %d", got, n)
		}
	}
	return got
}

func TestLiveFeed(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/live")
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	a := e.add(t, ep.ID, "{}", nil)
	b := e.add(t, ep.ID, "{}", nil)

	// Reconnect: Last-Event-ID wins over the page's "after".
	req, _ := http.NewRequest("GET", srv.URL+"/admin/e/"+id(ep.ID)+"/events?after=0", nil)
	req.Header.Set("Last-Event-ID", id(a.ID))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	evs := make(chan []string, 1)
	go func() { evs <- readEvents(t, resp.Body, 3) }()
	time.Sleep(100 * time.Millisecond)
	c := e.add(t, ep.ID, "{}", nil)
	e.lv.Publish(ep.ID)
	e.lv.Publish(ep.ID) // a second wake-up must not repeat c
	other, _ := e.st.CreateEndpoint(context.Background(), "/other")
	e.add(t, other.ID, "{}", nil)
	e.lv.Publish(other.ID)
	d := e.add(t, ep.ID, "{}", nil)
	e.lv.Publish(ep.ID)
	got := <-evs
	want := []string{id(b.ID), id(c.ID), id(d.ID)}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v, want %v: missed ones once each, then live ones once each, nothing from other endpoints", got, want)
	}
}

// TestLiveFeedSendsUpdatedOutcomes: a row already shown is sent again, as an
// "update" without an id, when its outcome changes (a delay measured, a
// render error, a timeout's wait).
func TestLiveFeedSendsUpdatedOutcomes(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/upd")
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	a := e.add(t, ep.ID, "{}", func(r *store.Request) { r.Outcome = `{"source":"default","action":"reply","status":200}` })
	resp, err := http.Get(srv.URL + "/admin/e/" + id(ep.ID) + "/events?after=" + id(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	time.Sleep(50 * time.Millisecond)
	e.st.UpdateOutcome(context.Background(), a.ID, 500, `{"source":"default","action":"error","status":500,"error":"boom"}`)
	e.lv.Publish(ep.ID)
	var event []string
	timeout := time.After(3 * time.Second)
	for {
		select {
		case l := <-lines:
			if l == "" && len(event) > 0 {
				joined := strings.Join(event, "\n")
				if strings.Contains(joined, "event: update") {
					if strings.Contains(joined, "id: ") {
						t.Fatal("an update must not carry an id (it would move Last-Event-ID)")
					}
					if !strings.Contains(joined, `id="req-`+id(a.ID)+`"`) || !strings.Contains(joined, "500 template error") {
						t.Fatalf("update event: %s", joined)
					}
					return
				}
				event = nil
				continue
			}
			if l != "" && !strings.HasPrefix(l, "retry:") {
				event = append(event, l)
			}
		case <-timeout:
			t.Fatal("no update event")
		}
	}
}

func TestStopStreamsEndsFeeds(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(e.h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/admin/e/unmatched/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	e.h.StopStreams()
	done := make(chan struct{})
	go func() { io.Copy(io.Discard, resp.Body); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("live feed still open after StopStreams")
	}
}

// TestAssetsSelfContained backs AC-8: every static file is served from the
// binary, and nothing the UI loads names another origin.
func TestAssetsSelfContained(t *testing.T) {
	e := newEnv(t)
	external := regexp.MustCompile(`(?i)(https?:)?//[a-z0-9.-]+\.[a-z]{2,}`)
	fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := strings.TrimPrefix(p, "static/")
		ext := filepath.Ext(name)
		if _, served := staticTypes[ext]; !served {
			return nil // licence and source notes ship in the binary, not served
		}
		rec := e.do("GET", e.h.assetURL(name), nil)
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
			t.Errorf("%s: %d %q", name, rec.Code, rec.Header().Get("Cache-Control"))
		}
		if ext == ".css" || ext == ".js" {
			if m := external.FindString(rec.Body.String()); m != "" {
				t.Errorf("%s refers to another origin: %s", name, m)
			}
		}
		return nil
	})
	for _, p := range []string{"/admin/", "/admin/e/unmatched"} {
		page := e.do("GET", p, nil).Body.String()
		for _, m := range regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(page, -1) {
			if !strings.HasPrefix(m[1], "/admin/") && !strings.HasPrefix(m[1], "data:") {
				t.Errorf("%s loads %s", p, m[1])
			}
		}
	}
	if csp := e.do("GET", "/admin/", nil).Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP %q", csp)
	}
	if rec := e.do("GET", "/admin/static/vendor/htmx/LICENSE", nil); rec.Code != 404 {
		t.Errorf("licence files are not served as assets: %d", rec.Code)
	}
}

// TestHostCheck: the UI answers to IP addresses, localhost and the configured
// names only, so a rebound DNS name cannot read it as same-origin.
func TestHostCheck(t *testing.T) {
	e := newEnv(t)
	for host, want := range map[string]int{
		"127.0.0.1:8080":          200,
		"[::1]:8080":              200,
		"192.0.2.10":              200,
		"localhost:8080":          200,
		"LOCALHOST":               200,
		"ui.localhost:8080":       200,
		"hooks.example.test:8443": 200,
		"hooks.example.test.":     200,
		"attacker.example:8080":   http.StatusMisdirectedRequest,
		"localhost.attacker.test": http.StatusMisdirectedRequest,
		"":                        http.StatusMisdirectedRequest,
	} {
		r := httptest.NewRequest("GET", "/admin/", nil)
		r.Host = host
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("Host %q: %d, want %d", host, rec.Code, want)
		}
	}
}

func TestSenderLeftShown(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/slow")
	r := e.add(t, ep.ID, `{}`, func(r *store.Request) {
		r.Status = 0
		r.Outcome = `{"source":"default","action":"abandoned","status":0,"would_status":200,"delay_ms":15000,"waited_ms":10003}`
	})
	base := "/admin/e/" + id(ep.ID)
	page := e.do("GET", base+"/r/"+id(r.ID), nil).Body.String()
	for _, want := range []string{
		`data-mark="timeout"`,
		`>sender left</span>`,
		"The sender left after 10.0 s, before the reply&#39;s 15.0 s delay ran out, so nothing was sent; it would have been 200",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("page lacks %q", want)
		}
	}
	md := e.do("GET", base+"/report?format=md&scope=all", nil).Body.String()
	if !strings.Contains(md, "| 0 | 0 | 0 | 1 | 0 | 0 |") || !strings.Contains(md, "sender left · default reply · left after 10.0 s of a 15.0 s delay; nothing sent, would have been 200") {
		t.Fatalf("report:\n%s", md)
	}
}

// A request answered 503 because the server was busy, and an Unmatched one
// of which only the head was kept, say so in the register and on their page.
func TestBusyAndHeadOnly(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/up")
	busy := e.add(t, ep.ID, "", func(r *store.Request) {
		r.TooLarge, r.Size, r.Status = true, 70000, 503
		r.Outcome = emulate.Outcome{Source: emulate.FromBusy, Action: emulate.ActReply, Status: 503}.String()
	})
	if reg := e.do("GET", "/admin/e/"+id(ep.ID), nil).Body.String(); !strings.Contains(reg, "Busy: 503, body not kept") {
		t.Error("the register does not say busy")
	}
	if ex := e.do("GET", "/admin/e/"+id(ep.ID)+"/r/"+id(busy.ID), nil).Body.String(); !strings.Contains(ex, "-max-inflight") ||
		strings.Contains(ex, "The sender tried to deliver") {
		t.Error("the page does not explain busy")
	}
	head := e.add(t, store.Unmatched, strings.Repeat("x", 64<<10), func(r *store.Request) { r.Path, r.Status, r.Size = "/nowhere", 404, 200<<10 })
	if ex := e.do("GET", "/admin/e/unmatched/r/"+id(head.ID), nil).Body.String(); !strings.Contains(ex, "Only the first 64.0 KiB of 200.0 KiB were kept") {
		t.Error("the page does not say only the head was kept")
	}
}
