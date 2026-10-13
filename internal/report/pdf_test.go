package report

import (
	"bytes"
	"os"
	"testing"
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
