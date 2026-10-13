package report

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/signintech/gopdf"

	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/picture"
)

// The PDF is drawn natively (no browser): A4 portrait, 15 mm margins, the
// recorder palette on white paper, Martian Mono for data (it has Cyrillic)
// and Atkinson Hyperlegible for Hookwitness's own captions (Latin only, so
// never for anything a request brought), both embedded as subsets. Every
// page has a running header and its number; a request starts on a new page
// rather than split before the end of its headers; long bodies wrap and
// continue across pages. Unlike Markdown and HTML, the document is kept in
// memory until it is written.

//go:embed fonts/martian-mono/*.ttf fonts/atkinson-hyperlegible/*.ttf
var pdfFonts embed.FS

const (
	pageW      = 595.28 // A4 in points
	pageH      = 841.89
	margin     = 42.52 // 15 mm
	contentW   = pageW - 2*margin
	bodyTop    = margin + 24
	bodyBottom = pageH - margin - 16

	pdfAscent = 0.92 // of the font size: Martian Mono's top-to-baseline at Cell

	fMono = "mono"
	fSans = "sans"
)

type rgb [3]uint8

var (
	cInk       = rgb{0x24, 0x30, 0x8C}
	cInkSoft   = rgb{0x4B, 0x56, 0xA6}
	cAlarm     = rgb{0xC2, 0x33, 0x1F}
	cText      = rgb{0x17, 0x20, 0x1B}
	cMuted     = rgb{0x58, 0x69, 0x5F}
	cRule      = rgb{0xC3, 0xD2, 0xC8}
	cPaper     = rgb{0xE6, 0xEC, 0xE7}
	cPaper2    = rgb{0xF0, 0xF4, 0xF1}
	cGrid      = rgb{0xB9, 0xCD, 0xBF}
	cGridMajor = rgb{0x93, 0xAF, 0x9C}
)

type pdfWriter struct {
	out   io.Writer
	pdf   *gopdf.GoPdf
	h     *Head
	y     float64
	pages int
	err   error
	adv   map[float64]float64 // mono advance per font size

	pending []block // drawn with the next request
}

func newPDF(w io.Writer) (writer, error) {
	p := &pdfWriter{out: w, pdf: &gopdf.GoPdf{}, adv: map[float64]float64{}}
	p.pdf.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4, Unit: gopdf.UnitPT})
	for _, f := range []struct {
		family, file string
		style        int
	}{
		{fMono, "fonts/martian-mono/MartianMono-StdRg.ttf", gopdf.Regular},
		{fMono, "fonts/martian-mono/MartianMono-StdBd.ttf", gopdf.Bold},
		{fSans, "fonts/atkinson-hyperlegible/AtkinsonHyperlegible-Regular.ttf", gopdf.Regular},
		{fSans, "fonts/atkinson-hyperlegible/AtkinsonHyperlegible-Bold.ttf", gopdf.Bold},
	} {
		b, err := pdfFonts.ReadFile(f.file)
		if err != nil {
			return nil, err
		}
		if err := p.pdf.AddTTFFontDataWithOption(f.family, b, gopdf.TtfOption{Style: f.style}); err != nil {
			return nil, fmt.Errorf("pdf font %s: %w", f.file, err)
		}
	}
	return p, nil
}

// Drawing primitives. Errors from the library are kept and reported once.

func (p *pdfWriter) keep(err error) {
	if err != nil && p.err == nil {
		p.err = err
	}
}

func (p *pdfWriter) font(family string, bold bool, size float64, c rgb) {
	style := gopdf.Regular
	if bold {
		style = gopdf.Bold
	}
	p.keep(p.pdf.SetFontWithStyle(family, style, size))
	p.pdf.SetTextColor(c[0], c[1], c[2])
}

// text draws s with its top at y.
func (p *pdfWriter) text(x, y float64, s string) {
	if s == "" {
		return
	}
	p.pdf.SetXY(x, y)
	p.keep(p.pdf.Cell(nil, s))
}

func (p *pdfWriter) width(s string) float64 {
	w, err := p.pdf.MeasureTextWidth(s)
	p.keep(err)
	return w
}

func (p *pdfWriter) stroke(c rgb, w float64) {
	p.pdf.SetStrokeColor(c[0], c[1], c[2])
	p.pdf.SetLineWidth(w)
	p.pdf.SetLineType("solid")
}

func (p *pdfWriter) fill(c rgb) { p.pdf.SetFillColor(c[0], c[1], c[2]) }

func (p *pdfWriter) rect(x, y, w, h float64, c rgb) {
	p.fill(c)
	p.pdf.RectFromUpperLeftWithStyle(x, y, w, h, "F")
}

// cols is how many characters of the mono font at size fit in w.
func (p *pdfWriter) cols(size, w float64) int {
	a, ok := p.adv[size]
	if !ok {
		p.font(fMono, false, size, cText)
		a = p.width("0000000000") / 10
		p.adv[size] = a
	}
	return max(8, int(w/a))
}

// A block is a piece of a page with a known height, drawn at a given top.
type block struct {
	h    float64
	draw func(y float64)
}

// newPage starts a page with the running header and the page number.
func (p *pdfWriter) newPage() {
	p.pdf.AddPage()
	p.pages++
	p.font(fSans, true, 7.5, cInk)
	p.text(margin, margin, "HOOKWITNESS REPORT")
	p.font(fMono, false, 7.5, cMuted)
	title := p.h.Endpoint + " · " + rangeLine(p.h)
	if w := p.width(title); w < contentW-100 {
		p.text(pageW-margin-w, margin, title)
	}
	p.stroke(cInk, 0.8)
	p.pdf.Line(margin, margin+12, pageW-margin, margin+12)
	p.font(fMono, false, 7.5, cMuted)
	label := "page " + strconv.Itoa(p.pages) + " of "
	p.text(pageW-margin-60-p.width(label), pageH-margin-8, label)
	// A placeholder is written like Text, from the baseline, not the top.
	p.pdf.SetXY(pageW-margin-60, pageH-margin-8+pdfAscent*7.5)
	p.keep(p.pdf.PlaceHolderText("pages", 60))
	p.font(fMono, false, 7.5, cMuted)
	p.text(margin, pageH-margin-8, "Generated "+stamp(p.h.Generated)+" by Hookwitness "+p.h.Version)
	p.y = bodyTop
}

// place draws b, on a new page when it does not fit.
func (p *pdfWriter) place(b block) {
	if p.y+b.h > bodyBottom && p.y > bodyTop {
		p.newPage()
	}
	b.draw(p.y)
	p.y += b.h
}

// together draws bs on one page when they fit on one.
func (p *pdfWriter) together(bs []block) {
	total := 0.0
	for _, b := range bs {
		total += b.h
	}
	if p.y+total > bodyBottom && p.y > bodyTop && total <= bodyBottom-bodyTop {
		p.newPage()
	}
	for _, b := range bs {
		p.place(b)
	}
}

func gapBlock(h float64) block { return block{h, func(float64) {}} }

// caption is a small upper-case label of Hookwitness's own.
func (p *pdfWriter) caption(s string) block {
	return block{14, func(y float64) {
		p.font(fSans, true, 7, cMuted)
		p.text(margin, y+3, strings.ToUpper(s))
	}}
}

// monoLines is s wrapped to w at the size, one block per line.
func (p *pdfWriter) monoLines(x, w, size float64, c rgb, bold bool, s string) []block {
	var out []block
	for _, line := range wrapRunes(s, p.cols(size, w)) {
		out = append(out, block{size * 1.45, func(y float64) {
			p.font(fMono, bold, size, c)
			p.text(x, y, line)
		}})
	}
	return out
}

// row is a label in the left column and wrapped mono text beside it.
func (p *pdfWriter) row(label string, labelW, size float64, c rgb, value string) []block {
	lines := wrapRunes(value, p.cols(size, contentW-labelW))
	h := float64(len(lines))*size*1.45 + 3
	return []block{{h, func(y float64) {
		p.font(fSans, true, 6.5, cMuted)
		p.text(margin, y+1.5, strings.ToUpper(label))
		p.font(fMono, false, size, c)
		for i, l := range lines {
			p.text(margin+labelW, y+float64(i)*size*1.45, l)
		}
	}}}
}

// kv is a two-column table of mono text, the name column 32 % wide.
func (p *pdfWriter) kv(rows [][2]string, masked []bool) []block {
	const size = 7.5
	nameW := contentW * 0.32
	var out []block
	for i, r := range rows {
		names := wrapRunes(r[0], p.cols(size, nameW-8))
		values := wrapRunes(r[1], p.cols(size, contentW-nameW))
		n := max(len(names), len(values))
		col := cText
		if masked != nil && masked[i] {
			col = cMuted
		}
		out = append(out, block{float64(n)*size*1.45 + 4, func(y float64) {
			p.font(fMono, false, size, cInk)
			for j, l := range names {
				p.text(margin, y+2+float64(j)*size*1.45, l)
			}
			p.font(fMono, false, size, col)
			for j, l := range values {
				p.text(margin+nameW, y+2+float64(j)*size*1.45, l)
			}
			p.stroke(cRule, 0.4)
			p.pdf.Line(margin, y+float64(n)*size*1.45+3.5, pageW-margin, y+float64(n)*size*1.45+3.5)
		}})
	}
	return out
}

func (p *pdfWriter) head(h *Head) error {
	p.h = h
	p.pdf.SetInfo(gopdf.PdfInfo{
		Title:        "Hookwitness report · " + h.Endpoint,
		Creator:      "Hookwitness " + h.Version,
		Producer:     "Hookwitness",
		CreationDate: h.Generated,
	})
	p.newPage()
	p.place(block{30, func(y float64) {
		p.font(fSans, true, 18, cInk)
		p.text(margin, y, "HOOKWITNESS")
		p.font(fMono, true, 13, cText)
		p.text(margin+p.widthAt(fSans, true, 18, "HOOKWITNESS")+14, y+3, "Report · "+h.Endpoint)
	}})
	masked := "none"
	if len(h.Masked) > 0 {
		masked = strings.Join(h.Masked, ", ")
	}
	for _, r := range [][2]string{
		{"Time range", rangeLine(h) + " (UTC)"},
		{"Selection", h.Selection},
		{"Requests", strconv.Itoa(h.Count)},
		{"Generated", stamp(h.Generated) + " by Hookwitness " + h.Version},
		{"Masked headers", masked},
	} {
		for _, b := range p.row(r[0], 92, 8.5, cText, r[1]) {
			p.place(b)
		}
	}
	p.place(gapBlock(8))
	p.place(p.stripBlock())
	p.place(block{24, func(y float64) {
		p.font(fSans, false, 7, cMuted)
		p.text(margin, y+4, "Height = body size · red: not 2xx · dashed: timeout · ×: dropped · red bar under the line: a failed check · same body bracketed · UTC")
	}})

	p.place(block{24, func(y float64) {
		p.font(fSans, true, 11, cInk)
		p.text(margin, y+6, "SUMMARY")
	}})
	p.place(p.caption("Answers"))
	colW := contentW / float64(len(h.Outcomes))
	p.place(block{30, func(y float64) {
		for i, c := range h.Outcomes {
			x := margin + float64(i)*colW
			p.font(fSans, false, 7.5, cMuted)
			p.text(x, y, c.Label)
			col := cText
			if c.N > 0 && c.Label != class2xx {
				col = cAlarm
			}
			p.font(fMono, true, 12, col)
			p.text(x, y+11, strconv.Itoa(c.N))
		}
	}})
	p.place(p.caption("Verdicts"))
	if len(h.Checks) == 0 {
		p.place(p.plain("No checks gave verdicts."))
	} else {
		cols := []string{"pass", "fail", "not checked", "not verifiable"}
		labelW := contentW - 4*62
		p.place(block{13, func(y float64) {
			p.font(fSans, false, 7.5, cMuted)
			p.text(margin, y, "Check")
			for i, c := range cols {
				p.text(margin+labelW+float64(i)*62, y, c)
			}
		}})
		for _, c := range h.Checks {
			label := strconv.Itoa(c.Check) + " · " + c.Label
			p.place(block{14, func(y float64) {
				p.font(fMono, false, 8, cText)
				p.text(margin, y, clip(label, p.cols(8, labelW-6)))
				for i, n := range []int{c.Pass, c.Fail, c.NC, c.NV} {
					col := cText
					if i == 1 && n > 0 {
						col = cAlarm
					}
					p.font(fMono, true, 8, col)
					p.text(margin+labelW+float64(i)*62, y, strconv.Itoa(n))
				}
				p.stroke(cRule, 0.4)
				p.pdf.Line(margin, y+11.5, pageW-margin, y+11.5)
			}})
		}
	}
	p.place(p.caption("Retries · the same body delivered more than once"))
	if len(h.Retries) == 0 {
		p.place(p.plain("None."))
	} else {
		var rows [][2]string
		for _, r := range h.Retries {
			var nos, gaps []string
			for _, n := range r.Nos {
				nos = append(nos, "#"+strconv.FormatInt(n, 10))
			}
			for _, g := range r.Gaps {
				gaps = append(gaps, gap(g))
			}
			rows = append(rows, [2]string{strings.Join(nos, ", "), strings.Join(gaps, ", ") + "  sha256 " + r.SHA256[:16] + "…"})
		}
		for _, b := range p.kv(rows, nil) {
			p.place(b)
		}
	}
	// The heading goes with the first request, never alone at a page end.
	p.pending = []block{gapBlock(10), {24, func(y float64) {
		p.font(fSans, true, 11, cInk)
		p.text(margin, y+6, "REQUESTS")
	}}}
	if h.Count == 0 {
		p.together(append(p.pending, p.plain("No requests in the selection.")))
		p.pending = nil
	}
	return p.err
}

func (p *pdfWriter) widthAt(family string, bold bool, size float64, s string) float64 {
	p.font(family, bold, size, cText)
	return p.width(s)
}

func (p *pdfWriter) plain(s string) block {
	return block{14, func(y float64) {
		p.font(fSans, false, 8.5, cMuted)
		p.text(margin, y, s)
	}}
}

// stripBlock draws the strip in vector.
func (p *pdfWriter) stripBlock() block {
	s := newStrip(p.h)
	const hgt = 104.0
	sx, sy := contentW/stripW, hgt/stripH
	return block{hgt + 2, func(y0 float64) {
		X := func(v float64) float64 { return margin + v*sx }
		Y := func(v float64) float64 { return y0 + v*sy }
		p.rect(margin, y0, contentW, hgt, cPaper)
		for _, g := range s.Grid {
			c := cGrid
			if g.Major {
				c = cGridMajor
			}
			p.stroke(c, 0.4)
			p.pdf.Line(X(g.X), Y(stripTop), X(g.X), Y(stripBase))
		}
		p.stroke(cGrid, 0.4)
		for _, r := range s.Rows {
			p.pdf.Line(margin, Y(r), margin+contentW, Y(r))
		}
		p.stroke(cGridMajor, 0.6)
		p.pdf.Line(margin, Y(stripBase), margin+contentW, Y(stripBase))
		p.font(fMono, false, 6, cMuted)
		for _, t := range s.Ticks {
			w := p.width(t.Label)
			p.text(min(max(X(t.X)-w/2, margin+2), margin+contentW-w-2), Y(stripH)-11, t.Label)
		}
		for _, b := range s.Brackets {
			p.stroke(cInkSoft, 0.6)
			p.pdf.SetCustomLineType([]float64{2, 2}, 0)
			p.pdf.Line(X(b.A), Y(b.Y), X(b.A), Y(b.Y)-4)
			p.pdf.Line(X(b.A), Y(b.Y)-4, X(b.B), Y(b.Y)-4)
			p.pdf.Line(X(b.B), Y(b.Y)-4, X(b.B), Y(b.Y))
			p.pdf.SetLineType("solid")
			p.font(fMono, true, 6, cInk)
			p.text(X(b.A)+1, Y(b.Y)-12, b.Label)
		}
		for _, m := range s.Marks {
			x, base, tip := X(m.X), Y(stripBase), Y(m.Tip)
			col := cInk
			if m.Kind != "ok" {
				col = cAlarm
			}
			if m.Failed {
				p.rect(x-3, base+2, 6, 2, cAlarm)
			}
			p.stroke(col, 1.2)
			switch m.Kind {
			case "timeout":
				p.pdf.SetCustomLineType([]float64{2, 2}, 0)
				p.pdf.Line(x, base, x, Y(stripTop))
				p.pdf.SetLineType("solid")
				p.pdf.Oval(x-2.5, Y(stripTop)-2.5, x+2.5, Y(stripTop)+2.5)
			case "drop":
				p.pdf.Line(x, base, x, tip)
				p.pdf.Line(x-3, tip-3, x+3, tip+3)
				p.pdf.Line(x+3, tip-3, x-3, tip+3)
			default:
				p.pdf.Line(x, base, x, tip)
				p.fill(col)
				p.pdf.Polygon(circle(x, tip, 2), "F")
			}
		}
	}}
}

// circle is a disc as a polygon.
func circle(x, y, r float64) []gopdf.Point {
	pts := make([]gopdf.Point, 0, 12)
	for _, d := range [][2]float64{{1, 0}, {.87, .5}, {.5, .87}, {0, 1}, {-.5, .87}, {-.87, .5}, {-1, 0}, {-.87, -.5}, {-.5, -.87}, {0, -1}, {.5, -.87}, {.87, -.5}} {
		pts = append(pts, gopdf.Point{X: x + d[0]*r, Y: y + d[1]*r})
	}
	return pts
}

func (p *pdfWriter) item(it Item) error {
	top := p.pending // kept together: title to the end of the headers
	p.pending = nil

	stampText := strings.ToUpper(it.Answer.Stamp)
	stampCol := cInk
	if !strings.HasPrefix(it.Answer.Stamp, "2") {
		stampCol = cAlarm
	}
	stampW := p.widthAt(fSans, true, 8, stampText) + 10
	titleLines := wrapRunes("#"+strconv.FormatInt(it.No, 10)+"  "+it.Method+" "+target(it), p.cols(10, contentW-stampW-16))
	top = append(top, gapBlock(6), block{float64(len(titleLines))*14 + 8, func(y float64) {
		p.rect(margin, y, contentW, float64(len(titleLines))*14+4, cPaper2)
		p.rect(margin, y, 2.5, float64(len(titleLines))*14+4, cInk)
		p.font(fMono, true, 10, cText)
		for i, l := range titleLines {
			p.text(margin+8, y+4+float64(i)*14, l)
		}
		p.stroke(stampCol, 1)
		p.pdf.RectFromUpperLeftWithStyle(pageW-margin-stampW-4, y+2.5, stampW, 13, "D")
		p.font(fSans, true, 8, stampCol)
		p.text(pageW-margin-stampW+1, y+5, stampText)
	}})
	top = append(top, p.row("Received", 70, 7.5, cText, stamp(it.At)+" from "+it.Remote+" · "+it.Proto)...)
	top = append(top, p.row("Answer", 70, 7.5, cText, answerLine(it.Answer))...)
	if it.Delivery > 1 {
		top = append(top, p.row("Delivery", 70, 7.5, cText, ordinal(it.Delivery)+" of this body (a retry)")...)
	}
	if len(it.Verdicts) > 0 {
		top = append(top, gapBlock(3), p.caption("Verdicts"))
		for _, v := range it.Verdicts {
			col := cText
			switch v.Result {
			case auth.Fail:
				col = cAlarm
			case auth.NotChecked, auth.NotVerifiable:
				col = cMuted
			}
			top = append(top, p.monoLines(margin, contentW, 7.5, col, true, v.Label+": "+string(v.Result))...)
			if v.Reason != "" {
				top = append(top, p.monoLines(margin+10, contentW-10, 7.5, col, false, v.Reason)...)
			}
			if v.Received != "" || v.Expected != "" {
				top = append(top, p.monoLines(margin+10, contentW-10, 7, cMuted, false, "received "+v.Received+" · expected "+v.Expected)...)
			}
			for _, pr := range v.Problems {
				top = append(top, p.monoLines(margin+10, contentW-10, 7, cText, false, pointer(pr)+"  "+pr.Keyword+"  "+pr.Message)...)
			}
			if more := v.Total - len(v.Problems); more > 0 {
				top = append(top, p.monoLines(margin+10, contentW-10, 7, cMuted, false, "and "+strconv.Itoa(more)+" more")...)
			}
		}
	}
	if len(it.Query) > 0 {
		top = append(top, gapBlock(3), p.caption("Query"))
		var rows [][2]string
		for _, q := range it.Query {
			rows = append(rows, [2]string{q.Name, q.Value})
		}
		top = append(top, p.kv(rows, nil)...)
	}
	top = append(top, gapBlock(3), p.caption("Headers"))
	if len(it.Headers) == 0 {
		top = append(top, p.plain("None."))
	} else {
		var rows [][2]string
		var masked []bool
		for _, hd := range it.Headers {
			rows = append(rows, [2]string{hd.Name, hd.Value})
			masked = append(masked, hd.Masked)
		}
		top = append(top, p.kv(rows, masked)...)
	}
	var pics []block // after the headers, before the body
	if len(it.Images) > 0 {
		pics = append(pics, gapBlock(3), p.caption("Images"))
		for _, img := range it.Images {
			col := cMuted
			if img.Error != "" {
				col = cAlarm
			}
			pics = append(pics, p.monoLines(margin, contentW, 7.5, cText, true, imageLabel(img.Found))...)
			pics = append(pics, p.monoLines(margin, contentW, 7, col, false, imageNote(img.Found))...)
			if b, ok := p.picture(img); ok {
				pics = append(pics, b)
			}
		}
	}
	note := wrapRunes(bodyNote(it.Body), p.cols(7, contentW-30))
	bodyStart := []block{gapBlock(3), {float64(len(note))*10 + 4, func(y float64) {
		p.font(fSans, true, 7, cMuted)
		p.text(margin, y+3, "BODY")
		p.font(fMono, false, 7, cMuted)
		for i, l := range note {
			p.text(margin+30, y+3+float64(i)*10, l)
		}
	}}}
	lines := p.bodyLines(it.Body)
	if len(lines) > 0 {
		bodyStart = append(bodyStart, lines[0]) // the body never starts alone on the next page
		lines = lines[1:]
	}
	if len(pics) == 0 {
		p.together(append(top, bodyStart...))
	} else {
		p.together(top)
		for _, b := range pics {
			p.place(b)
		}
		p.together(bodyStart)
	}
	for _, b := range lines {
		p.place(b)
	}
	if it.Body.Cut {
		p.place(p.plain("Cut at " + size(int64(it.Body.Shown)) + "."))
	}
	p.place(gapBlock(10))
	return p.err
}

// picture is an image drawn to fit the width (at most 260 pt high, never
// enlarged): PNG and JPEG as sent, GIF and WebP decoded first, since a PDF
// cannot hold them.
func (p *pdfWriter) picture(img ItemImage) (block, bool) {
	if len(img.Data) == 0 || img.Width <= 0 || img.Height <= 0 {
		return block{}, false
	}
	const maxH = 260.0
	w, h := float64(img.Width)*0.75, float64(img.Height)*0.75 // 96 dpi pixels to points
	if w > contentW {
		w, h = contentW, h*contentW/w
	}
	if h > maxH {
		w, h = w*maxH/h, maxH
	}
	var holder gopdf.ImageHolder
	var decoded image.Image
	switch img.Type {
	case picture.PNG, picture.JPEG:
		hd, err := gopdf.ImageHolderByBytes(img.Data)
		if err != nil {
			return block{}, false
		}
		holder = hd
	default:
		im, _, err := image.Decode(bytes.NewReader(img.Data))
		if err != nil {
			return block{}, false
		}
		decoded = im
	}
	return block{h + 8, func(y float64) {
		p.stroke(cRule, 0.5)
		p.pdf.RectFromUpperLeftWithStyle(margin, y+2, w, h, "D")
		if holder != nil {
			p.keep(p.pdf.ImageByHolder(holder, margin, y+2, &gopdf.Rect{W: w, H: h}))
		} else {
			p.keep(p.pdf.ImageFrom(decoded, margin, y+2, &gopdf.Rect{W: w, H: h}))
		}
	}}, true
}

// seg is a run of body text in one colour.
type seg struct {
	text string
	col  rgb
}

// bodyLines lays a body out as mono lines on paper, JSON in colour, each
// line wrapped to the width.
func (p *pdfWriter) bodyLines(b Body) []block {
	const size = 7
	var lines [][]seg
	switch b.Kind {
	case BodyJSON:
		lines = [][]seg{nil}
		for _, t := range b.Tokens {
			col := cText
			switch {
			case t.Bad:
				col = cAlarm
			case t.Class == "k":
				col = cInk
			case t.Class == "n":
				col = cInkSoft
			}
			for i, part := range strings.Split(t.Text, "\n") {
				if i > 0 {
					lines = append(lines, nil)
				}
				if part != "" {
					lines[len(lines)-1] = append(lines[len(lines)-1], seg{part, col})
				}
			}
		}
	case BodyText:
		text := plainLines.Replace(strings.TrimRight(b.Text, "\n"))
		for _, l := range strings.Split(text, "\n") {
			lines = append(lines, []seg{{l, cText}})
		}
	default:
		return nil
	}
	cols := p.cols(size, contentW-12)
	adv := p.adv[size] // a mono font: every character is as wide
	var out []block
	for _, l := range lines {
		for _, w := range wrapSegs(l, cols) {
			out = append(out, block{size * 1.5, func(y float64) {
				p.rect(margin, y, contentW, size*1.5, cPaper2)
				x := margin + 6
				for _, s := range w {
					p.font(fMono, false, size, s.col)
					p.text(x, y+1, s.text)
					x += adv * float64(utf8.RuneCountInString(s.text))
				}
			}})
		}
	}
	return out
}

func (p *pdfWriter) end() error {
	if p.err != nil {
		return p.err
	}
	p.font(fMono, false, 7.5, cMuted)
	if err := p.pdf.FillInPlaceHoldText("pages", strconv.Itoa(p.pages), gopdf.Left); err != nil {
		return err
	}
	_, err := p.pdf.WriteTo(p.out)
	return err
}

// plainLines turns any line break into \n and a tab into four spaces.
var plainLines = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\t", "    ")

// wrapRunes breaks s into lines of at most n characters, at spaces when a
// line has one in its second half, anywhere otherwise; line breaks in s
// are kept.
func wrapRunes(s string, n int) []string {
	var out []string
	for _, line := range strings.Split(plainLines.Replace(s), "\n") {
		rs := []rune(line)
		for len(rs) > n {
			cut := n
			for i := n; i > n/2; i-- {
				if rs[i] == ' ' {
					cut = i
					break
				}
			}
			out = append(out, strings.TrimRight(string(rs[:cut]), " "))
			rs = rs[cut:]
			for len(rs) > 0 && rs[0] == ' ' && cut != n {
				rs = rs[1:]
			}
		}
		out = append(out, string(rs))
	}
	return out
}

// wrapSegs breaks a coloured line into lines of at most n characters.
func wrapSegs(l []seg, n int) [][]seg {
	out := [][]seg{nil}
	used := 0
	for _, s := range l {
		text := s.text
		for text != "" {
			room := n - used
			if room == 0 {
				out = append(out, nil)
				used, room = 0, n
			}
			k := utf8.RuneCountInString(text)
			if k <= room {
				out[len(out)-1] = append(out[len(out)-1], seg{text, s.col})
				used += k
				break
			}
			i := byteIndexOfRune(text, room)
			out[len(out)-1] = append(out[len(out)-1], seg{text[:i], s.col})
			used += room
			text = text[i:]
		}
	}
	return out
}

func byteIndexOfRune(s string, n int) int {
	i := 0
	for k := range s {
		if i == n {
			return k
		}
		i++
	}
	return len(s)
}

// clip shortens s to n characters with an ellipsis.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
