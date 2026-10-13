package web

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/store"
)

func TestReportDialog(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/orders")
	e.edit(ep.ID, url.Values{"op": {"secret-save"}, "i": {"new"}, "name": {"GH"}, "secret_kind": {"key"}, "value": {"whsec_test_plain_9"}})
	e.edit(ep.ID, url.Values{"op": {"check-save"}, "i": {"new"}, "kind": {"github"}, "secret": {"GH"}})
	r := e.add(t, ep.ID, `{"a":1}`, nil)
	base := "/admin/e/" + id(ep.ID)

	page := e.do("GET", base, nil).Body.String()
	if !strings.Contains(page, `href="`+base+`?report"`) {
		t.Fatal("no Report button on the register")
	}
	dialog := e.do("GET", base+"?report", nil).Body.String()
	for _, want := range []string{
		`<form class="card" method="get" action="` + base + `/report">`,
		`<option value="all" selected>All 1 kept</option>`,
		`value="Authorization"`, `value="Cookie"`, `value="Set-Cookie"`, `value="X-Hub-Signature-256"`,
	} {
		if !strings.Contains(dialog, want) {
			t.Fatalf("dialog lacks %s", want)
		}
	}
	if strings.Contains(dialog, `name="mask" value="Authorization" checked`) {
		t.Fatal("nothing is masked by default")
	}
	onExhibit := e.do("GET", base+"/r/"+id(r.ID)+"?report", nil).Body.String()
	if !strings.Contains(onExhibit, `<option value="picked" selected>`) || !strings.Contains(onExhibit, `name="pick" value="1"`) {
		t.Fatal("from an exhibit the dialog does not pick that request")
	}
}

func TestReportDownload(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, _ := e.st.CreateEndpoint(ctx, "/orders")
	base := "/admin/e/" + id(ep.ID)
	t0 := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for i := range 5 {
		e.add(t, ep.ID, `{"n":`+string(rune('0'+i))+`}`, func(r *store.Request) {
			r.ReceivedAt = t0.Add(time.Duration(i) * time.Minute)
			r.Headers = append(r.Headers, store.Header{Name: "Authorization", Value: "Bearer tok-" + string(rune('a'+i))})
		})
	}
	for format, ctype := range map[string]string{"md": "text/markdown; charset=utf-8", "html": "text/html; charset=utf-8", "pdf": "application/pdf"} {
		rec := e.do("GET", base+"/report?format="+format+"&scope=all", nil)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != ctype {
			t.Fatalf("%s: %d %s", format, rec.Code, rec.Header().Get("Content-Type"))
		}
		cd := rec.Header().Get("Content-Disposition")
		if !regexp.MustCompile(`^attachment; filename="hookwitness-orders-\d{8}T\d{6}Z\.` + format + `"$`).MatchString(cd) {
			t.Fatalf("%s: %s", format, cd)
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("%s: empty", format)
		}
	}
	html := e.do("GET", base+"/report?format=html&scope=all", nil).Body.String()
	if !strings.Contains(html, "font/woff2;base64,") || !strings.Contains(html, "default-src 'none'") {
		t.Fatal("the HTML report does not embed its fonts or forbid requests")
	}

	md := e.do("GET", base+"/report?format=md&scope=range&from=2026-10-11T12:01:00.000&to=2026-10-11T12:03:00&mask=Authorization", nil).Body.String()
	if !strings.Contains(md, "| Requests | 3 |") || !strings.Contains(md, "| Selection | received from 2026-10-11T12:01:00.000Z to 2026-10-11T12:03:00.000Z |") {
		t.Fatalf("range:\n%s", md[:min(600, len(md))])
	}
	if strings.Contains(md, "tok-b") || !strings.Contains(md, "•••• 12 chars") {
		t.Fatal("Authorization not masked")
	}
	picked := e.do("GET", base+"/report?format=md&scope=picked&pick=1,+4-5&mask_other=Authorization", nil).Body.String()
	if !strings.Contains(picked, "| Requests | 3 |") || !strings.Contains(picked, "### #4 ") || strings.Contains(picked, "### #2 ") || strings.Contains(picked, "tok-") {
		t.Fatal("picked by number")
	}
	if one := e.do("GET", base+"/report?format=md&scope=picked&pick=2", nil).Body.String(); !strings.Contains(one, "| Selection | request #2 |") {
		t.Fatal("one request")
	}

	for q, want := range map[string]string{
		"format=doc&scope=all":                                            "choose Markdown, HTML or PDF",
		"format=md&scope=range":                                           "give the start, the end or both",
		"format=md&scope=range&from=yesterday":                            "the start is not a date and time",
		"format=md&scope=range&from=2026-10-11T12:05&to=2026-10-11T12:01": "the end is before the start",
		"format=md&scope=picked&pick=3-1":                                 `&#34;3-1&#34; is not a request number`,
		"format=md&scope=picked&pick=1-5000":                              "at most 1000 requests",
		"format=md&scope=all&mask_other=Bad+Name":                         `&#34;Bad Name&#34; is not a header name`,
	} {
		rec := e.do("GET", base+"/report?"+q, nil)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "No report: "+want) {
			t.Fatalf("%s: %d, want the dialog with %q", q, rec.Code, want)
		}
		if !strings.Contains(rec.Body.String(), `aria-invalid`) {
			t.Fatalf("%s: the field is not marked", q)
		}
	}
	if rec := e.do("GET", "/admin/e/999/report?format=md&scope=all", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown endpoint: %d", rec.Code)
	}
}

func TestReportOfUnmatched(t *testing.T) {
	e := newEnv(t)
	e.add(t, store.Unmatched, "", func(r *store.Request) { r.Path = "/typo"; r.Status = 404 })
	rec := e.do("GET", "/admin/e/unmatched/report?format=md&scope=all", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Disposition"), "hookwitness-unmatched-") ||
		!bytes.Contains(rec.Body.Bytes(), []byte("# Hookwitness report · Unmatched")) {
		t.Fatalf("%d %s", rec.Code, rec.Header().Get("Content-Disposition"))
	}
}

func TestReportImagesOption(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/pics")
	e.add(t, ep.ID, string(pngBytes(t, 4, 4)), func(r *store.Request) {
		r.Headers = []store.Header{{Name: "Content-Type", Value: "image/png"}}
	})
	base := "/admin/e/" + id(ep.ID)
	if !strings.Contains(e.do("GET", base+"?report", nil).Body.String(), `<option value="shown" selected>`) {
		t.Fatal("images are shown by default")
	}
	shown := e.do("GET", base+"/report?format=html&scope=all", nil).Body.String()
	listed := e.do("GET", base+"/report?format=html&scope=all&images=listed", nil).Body.String()
	if !strings.Contains(shown, `<img src="data:image/png;base64,`) || strings.Contains(listed, `<img src="data:image/png;base64,`) ||
		!strings.Contains(listed, "<code>the body</code>") {
		t.Fatal("the images option")
	}
}
