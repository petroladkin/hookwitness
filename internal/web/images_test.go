package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/petroladkin/hookwitness/internal/store"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExhibitImages(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/uploads")
	e.edit(ep.ID, url.Values{"op": {"schema-save"}, "i": {"new"}, "name": {"Upload"}, "text": {
		`{"type":"object","properties":{"photo":{"type":"string","contentEncoding":"base64","contentMediaType":"image/png"},
		  "scan":{"type":"string","contentEncoding":"base64","contentMediaType":"image/png"}}}`}})
	e.edit(ep.ID, url.Values{"op": {"check-save"}, "i": {"new"}, "kind": {"schema"}, "schema": {"Upload"}})
	pic := base64.StdEncoding.EncodeToString(pngBytes(t, 6, 4))
	body := `{"photo":"` + pic + `","scan":"aGVsbG8=","extra":"data:image/png;base64,` + pic + `"}`
	r := e.add(t, ep.ID, body, nil)
	base := "/admin/e/" + id(ep.ID)
	e.do("POST", base+"/recheck", url.Values{})

	page := e.do("GET", base+"/r/"+id(r.ID), nil).Body.String()
	img := "/admin/r/" + id(r.ID) + "/image?path=%2Fphoto"
	for _, want := range []string{
		"<h2>Images · 3</h2>",
		`<img src="` + img + `" alt="The image at /photo"`,
		`PNG · 90 B · 6×4 px · declared image/png`,
		`/extra</code> <span class="note">PNG · 90 B · 6×4 px · a data URL, no schema`,
		`not a PNG, JPEG, GIF, WebP, HEIC or AVIF image`,
		`<span class="s img">`,
		`<dialog class="viewer" id="viewer"`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("exhibit lacks %q", want)
		}
	}

	rec := e.do("GET", img, nil)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" || !bytes.Equal(rec.Body.Bytes(), pngBytes(t, 6, 4)) ||
		rec.Header().Get("Content-Disposition") != `inline; filename="request-`+id(r.ID)+`-photo.png"` {
		t.Fatalf("image: %d %v", rec.Code, rec.Header())
	}
	if cd := e.do("GET", img+"&download=1", nil).Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
		t.Fatalf("download: %s", cd)
	}
	for q, code := range map[string]int{
		"?path=%2Fscan":  http.StatusUnsupportedMediaType, // bytes that are no image are never served
		"?path=%2Fnope":  http.StatusNotFound,
		"?path=photo":    http.StatusNotFound,
		"?path=%2Fextra": http.StatusOK,
		"?path=":         http.StatusUnsupportedMediaType, // the body is JSON
	} {
		if got := e.do("GET", "/admin/r/"+id(r.ID)+"/image"+q, nil).Code; got != code {
			t.Errorf("%s: %d, want %d", q, got, code)
		}
	}
}

func TestBodyThatIsAnImage(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(context.Background(), "/raw")
	r := e.add(t, ep.ID, string(pngBytes(t, 3, 3)), func(r *store.Request) {
		r.Headers = []store.Header{{Name: "Content-Type", Value: "image/png"}}
	})
	page := e.do("GET", "/admin/e/"+id(ep.ID)+"/r/"+id(r.ID), nil).Body.String()
	if !strings.Contains(page, `<code>the body</code>`) || !strings.Contains(page, `/image?path=" alt="The image at the body"`) {
		t.Fatal("a body that is an image has no preview")
	}
	if rec := e.do("GET", "/admin/r/"+id(r.ID)+"/image?path=", nil); rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("%d", rec.Code)
	}
	heic := e.add(t, ep.ID, "\x00\x00\x00\x18ftypheic"+strings.Repeat("\x00", 16), nil)
	page = e.do("GET", "/admin/e/"+id(ep.ID)+"/r/"+id(heic.ID), nil).Body.String()
	if !strings.Contains(page, "cannot be shown here (image/heic); download it") || strings.Contains(page, `class="thumb" data-full`) {
		t.Fatal("HEIC")
	}
	if got := e.do("GET", "/admin/r/"+id(heic.ID)+"/image?path=", nil).Code; got != http.StatusUnsupportedMediaType {
		t.Fatalf("HEIC inline: %d", got)
	}
	if rec := e.do("GET", "/admin/r/"+id(heic.ID)+"/image?path=&download=1", nil); rec.Code != 200 || rec.Header().Get("Content-Type") != "image/heic" {
		t.Fatalf("HEIC download: %d", rec.Code)
	}
}
