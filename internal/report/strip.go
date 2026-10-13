package report

import (
	"math"
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
	stripTop  = 30.0
	stripBase = stripH - 26
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
	A, B, Y float64
	Label   string
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
			s.Brackets = append(s.Brackets, stripBracket{
				A: round(x(prev.At)), B: mx, Y: stripTop - 6,
				Label: gap(m.At.Sub(prev.At)),
			})
		}
		last[m.Group] = i
	}
	return s
}

func round(v float64) float64 { return math.Round(v*10) / 10 }

// num formats a strip coordinate for SVG.
func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
