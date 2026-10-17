package report

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"time"
)

// The strip of a report is the UI's recorder strip over the reported range
// instead of the last two minutes: a grid with time ticks, a mark per
// request (height = body size, log scale; red when not 2xx, a dashed stem
// for a timeout, a cross for a drop, a red bar under the line for a failed
// check) and the deliveries of one body bracketed with their gap. Its
// geometry is computed once, in strip units, and drawn by each format.

const (
	stripW    = 1000.0
	stripH    = 132.0
	stripPad  = 24.0
	stripTop  = 40.0 // below the two bracket rows and their labels
	stripBase = stripH - 26

	stripRows   = 2    // bracket rows
	stripRowGap = 12.0 // between bracket rows
	stripChar   = 7.0  // a label character's width, in strip units (HTML 10 px, PDF 6 pt mono)
)

type strip struct {
	Grid     []stripLine // vertical grid lines
	Rows     []float64   // horizontal grid lines (y)
	Ticks    []stripTick
	Marks    []stripMark
	Brackets []stripBracket
}

type stripLine struct {
	X     float64
	Major bool
}

type stripTick struct {
	X     float64
	Label string
}

type stripMark struct {
	X, Tip float64
	Kind   string
	Failed bool
}

type stripBracket struct {
	A, B, Y float64 // Y: where its ticks end; its line is 6 above
	Label   string  // "" when no place fits it
	LX      float64 // the label's centre
	Dense   bool    // no free row: drawn over another, dotted
	group   int
	gap     time.Duration
}

// tickSteps are the grid spacings a strip chooses from.
var tickSteps = []time.Duration{
	100 * time.Millisecond, 200 * time.Millisecond, 500 * time.Millisecond,
	time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second,
	time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
	2 * 24 * time.Hour, 7 * 24 * time.Hour, 14 * 24 * time.Hour, 28 * 24 * time.Hour,
}

func newStrip(h *Head) strip {
	var s strip
	from, to := h.From, h.To
	if to.Sub(from) < 2*time.Second {
		mid := from.Add(to.Sub(from) / 2)
		from, to = mid.Add(-time.Second), mid.Add(time.Second)
	}
	span := to.Sub(from)
	x := func(t time.Time) float64 {
		return stripPad + float64(t.Sub(from))/float64(span)*(stripW-2*stripPad)
	}
	// About eight labelled ticks, a grid line at a fifth of a tick.
	step := tickSteps[len(tickSteps)-1]
	for _, st := range tickSteps {
		if span/st <= 8 {
			step = st
			break
		}
	}
	minor := step / 5
	if minor < 20*time.Millisecond {
		minor = step
	}
	for t := from.Truncate(minor); !t.After(to); t = t.Add(minor) {
		if t.Before(from) {
			continue
		}
		s.Grid = append(s.Grid, stripLine{round(x(t)), t.Equal(t.Truncate(step))})
	}
	for y := stripBase; y >= stripTop-10; y -= 16 {
		s.Rows = append(s.Rows, y)
	}
	layout := "15:04:05"
	switch {
	case step < time.Second:
		layout = "15:04:05.0"
	case span > 24*time.Hour:
		layout = "Jan 2 15:04"
	}
	for t := from.Truncate(step); !t.After(to); t = t.Add(step) {
		if t.Before(from) {
			continue
		}
		s.Ticks = append(s.Ticks, stripTick{round(x(t)), t.UTC().Format(layout)})
	}

	last := map[int]int{} // group → index of its previous mark
	for i, m := range h.Marks {
		mx := round(x(m.At))
		sz := float64(m.Size)
		if sz <= 0 {
			sz = 64
		}
		hgt := math.Max(10, math.Min(stripBase-stripTop, (math.Log2(math.Max(sz, 64))-6)*9))
		s.Marks = append(s.Marks, stripMark{X: mx, Tip: round(stripBase - hgt), Kind: m.Kind, Failed: m.Failed})
		if m.Group == 0 {
			continue
		}
		if p, ok := last[m.Group]; ok {
			prev := h.Marks[p]
			s.Brackets = append(s.Brackets, stripBracket{A: round(x(prev.At)), B: mx, group: m.Group, gap: m.At.Sub(prev.At)})
		}
		last[m.Group] = i
	}
	placeBrackets(s.Brackets)
	return s
}

// placeBrackets puts brackets on rows and their gap labels where they fit,
// as the UI's strip does (internal/web/static/strip.js): the lowest row free
// (or holding the same body's previous bracket that ends where this one
// starts), else the least crowded; a label inside its bracket in full or
// short, else beside it, never over another label of its row, else none.
func placeBrackets(bs []stripBracket) {
	slices.SortStableFunc(bs, func(a, b stripBracket) int { return cmp.Compare(a.A, b.A) })
	ends := []float64{math.Inf(-1), math.Inf(-1)}
	groups := []int{0, 0}
	rowOf := make([]int, len(bs))
	for i := range bs {
		b := &bs[i]
		row := -1
		for r := 0; r < stripRows && row < 0; r++ {
			if ends[r] < b.A-6 || (groups[r] == b.group && ends[r] == b.A) {
				row = r
			}
		}
		if row < 0 {
			row = 0
			for r := 1; r < stripRows; r++ {
				if ends[r] < ends[row] {
					row = r
				}
			}
			b.Dense = true
		}
		ends[row], groups[row] = math.Max(ends[row], b.B), b.group
		rowOf[i] = row
		b.Y = stripTop - 8 - stripRowGap*float64(row)
	}
	taken := make([][][2]float64, stripRows)
	free := func(row int, l, r float64) bool {
		if l < 2 || r > stripW-2 {
			return false
		}
		for _, t := range taken[row] {
			if !(r+3 <= t[0] || l >= t[1]+3) {
				return false
			}
		}
		return true
	}
	order := make([]int, len(bs))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(i, j int) int { return cmp.Compare(bs[j].B, bs[i].B) }) // newest first
	for _, i := range order {
		b := &bs[i]
		row := rowOf[i]
		long, brief := gap(b.gap), shortGap(b.gap)
		type try struct {
			text   string
			l      float64
			inside bool
		}
		var tries []try
		for _, t := range []string{long, brief} {
			wd := stripChar * float64(len(t))
			tries = append(tries, try{t, (b.A+b.B)/2 - wd/2, true})
		}
		for _, t := range []string{long, brief} {
			wd := stripChar * float64(len(t))
			tries = append(tries, try{t, b.B + 3, false}, try{t, b.A - 3 - wd, false})
		}
		for _, t := range tries {
			wd := stripChar * float64(len(t.text))
			if t.inside && wd+6 > b.B-b.A {
				continue
			}
			if !free(row, t.l, t.l+wd) {
				continue
			}
			b.Label, b.LX = t.text, round(t.l+wd/2)
			taken[row] = append(taken[row], [2]float64{t.l, t.l + wd})
			break
		}
	}
}

func round(v float64) float64 { return math.Round(v*10) / 10 }

// num formats a strip coordinate for SVG.
func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// shortGap is a bracket's gap in few characters: "1.5s", "12s".
func shortGap(d time.Duration) string {
	if sec := d.Seconds(); sec < 10 {
		return strconv.FormatFloat(sec, 'f', 1, 64) + "s"
	}
	return strconv.Itoa(int(math.Round(d.Seconds()))) + "s"
}
