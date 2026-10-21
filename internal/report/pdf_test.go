package report

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/picture"
	"github.com/petroladkin/hookwitness/internal/store"
)

func TestPDF(t *testing.T) {
	var buf bytes.Buffer
	o := goldenOptions()
	if err := Write(t.Context(), &buf, PDF, o, sliceSource(fixture(t))); err != nil {
		t.Fatal(err)
	}
	if out := os.Getenv("REPORT_PDF_OUT"); out != "" {
		os.WriteFile(out, buf.Bytes(), 0o644)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("%PDF-")) {
		t.Fatal("not a PDF")
	}
}

// TestPDFGlyphsTheFontsLack: characters Martian Mono has no glyph for
// (emoji, CJK) do not fail the report.
func TestPDFGlyphsTheFontsLack(t *testing.T) {
	rs := fixture(t)[:1]
	rs[0].Body = []byte(`{"note":"emoji 😀, 中文, ünïcödé, Ґанок"}`)
	var buf bytes.Buffer
	if err := Write(t.Context(), &buf, PDF, goldenOptions(), sliceSource(rs)); err != nil {
		t.Fatal(err)
	}
}

func TestPDFStructure(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(t.Context(), &buf, PDF, goldenOptions(), sliceSource(fixture(t))); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	pages := bytes.Count(b, []byte("/Type /Page\n"))
	if fonts := bytes.Count(b, []byte("/FontFile2")); fonts != 4 {
		t.Errorf("%d embedded fonts, want 4 (mono and sans, regular and bold)", fonts)
	}
	if !bytes.Contains(b, []byte("/MediaBox [ 0 0 595.00 842.00 ]")) {
		t.Error("not A4")
	}
	if pages != 4 {
		t.Errorf("%d pages, want 4", pages)
	}
}

// Images a PDF writer cannot take as they are (16-bit, a palette with
// transparency) are drawn after decoding; one that cannot be drawn leaves a
// line instead of failing the report.
func TestPDFImageKinds(t *testing.T) {
	r := image.Rect(0, 0, 40, 30)
	opaque := image.NewRGBA64(r)
	for i := range opaque.Pix {
		opaque.Pix[i] = 0xff
	}
	pal := image.NewPaletted(r, color.Palette{color.Transparent, color.Black})
	var fields []string
	for i, img := range []image.Image{image.NewGray16(r), image.NewNRGBA64(r), opaque, pal, image.NewGray(r)} {
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			t.Fatal(err)
		}
		fields = append(fields, fmt.Sprintf(`"i%d":"data:image/png;base64,%s"`, i, base64.StdEncoding.EncodeToString(b.Bytes())))
	}
	req := store.Request{ID: 1, No: 1, Method: "POST", Path: "/p", Status: 200, ReceivedAt: time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC),
		Headers: []store.Header{{Name: "Content-Type", Value: "application/json"}}, Body: []byte("{" + strings.Join(fields, ",") + "}")}
	req.Size = int64(len(req.Body))
	var buf bytes.Buffer
	if err := Write(t.Context(), &buf, PDF, goldenOptions(), sliceSource([]store.Request{req})); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "/Subtype /Image"); n != 5 {
		t.Fatalf("%d images drawn, want 5", n)
	}

	// Drawing fails: the frame says so, and the report does not fail.
	w, err := newPDF(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	p := w.(*pdfWriter)
	p.pdf.AddPage()
	half := image.NewNRGBA(image.Rect(0, 0, 10, 10)) // with alpha: the writer inflates it when it draws
	for i := range half.Pix {
		half.Pix[i] = 0x80
	}
	var good bytes.Buffer
	png.Encode(&good, half)
	broken := bytes.Clone(good.Bytes())
	broken[len(broken)-20] ^= 0xff // inside the compressed pixels: "zlib: invalid checksum" when drawn
	for name, data := range map[string][]byte{"undecodable": []byte("RIFF....WEBPVP8 nope"), "fails to draw": broken} {
		typ := picture.WebP
		if name == "fails to draw" {
			typ = picture.PNG
		}
		b, ok := p.picture(ItemImage{Found: picture.Found{Image: auth.Image{Type: typ, Width: 10, Height: 10}}, Data: data})
		if !ok {
			t.Fatalf("%s: no frame", name)
		}
		b.draw(100)
		if p.err != nil {
			t.Fatalf("%s: the report fails: %v", name, p.err)
		}
	}
}

// TestPDFFooterFits: the footer's "Generated <time> · <version>" line never
// runs into "page N of M", however long the version; a release's and a dev
// build's describe string are drawn whole, a longer one is shortened.
func TestPDFFooterFits(t *testing.T) {
	type drawnText struct {
		x, y, w float64
		s       string
	}
	for _, c := range []struct {
		version string
		whole   bool
	}{
		{"v1.0.0 (0123abc)", true},
		{"v0.1.0-rc.1-55-g8a1e8e0 (8a1e8e0)", true},
		{"v1.0.0-rc.12-1234-g0123456789ab-dirty-with-a-very-long-local-suffix (0123456789ab)", false},
	} {
		version := c.version
		var drawn []drawnText
		pdfTextHook = func(x, y, w float64, s string) { drawn = append(drawn, drawnText{x, y, w, s}) }
		o := goldenOptions()
		o.Version = version
		render(t, PDF, o, fixture(t))
		pdfTextHook = nil

		var footers, labels []drawnText
		for _, d := range drawn {
			switch {
			case strings.HasPrefix(d.s, "Generated "):
				footers = append(footers, d)
			case strings.HasPrefix(d.s, "page "):
				labels = append(labels, d)
			}
		}
		if len(footers) < 2 || len(footers) != len(labels) {
			t.Fatalf("%s: %d footers and %d page labels, want one of each on every page (several pages)", version, len(footers), len(labels))
		}
		for i, f := range footers {
			if gap := labels[i].x - (f.x + f.w); gap < 8 {
				t.Errorf("%s: page %d: the footer ends %.1f pt before the page number, want at least 8", version, i+1, gap)
			}
			whole := strings.HasSuffix(f.s, " · "+version)
			if whole != c.whole {
				t.Errorf("%s: page %d: footer %q (whole: %v)", version, i+1, f.s, whole)
			}
			if !whole && !strings.HasSuffix(f.s, "…") {
				t.Errorf("%s: page %d: a shortened footer %q does not end with an ellipsis", version, i+1, f.s)
			}
		}
	}
}

// TestPDFRepeatNote: a verdict's note (the same signature as an earlier
// request's) is drawn under it.
func TestPDFRepeatNote(t *testing.T) {
	var drawn []string
	pdfTextHook = func(_, _, _ float64, s string) { drawn = append(drawn, s) }
	defer func() { pdfTextHook = nil }()
	render(t, PDF, goldenOptions(), fixture(t))
	all := strings.Join(drawn, "\n")
	if !strings.Contains(all, "same signature as #1: a replay, or a") {
		t.Fatalf("the note is not drawn:\n%s", all)
	}
}
