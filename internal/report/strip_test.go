package report

import (
	"testing"
	"time"
)

// The report's strip places brackets as the UI's does: interleaved retries
// on different rows, a chain on one, labels never overlapping in a row,
// every bracket and label above the marks.
func TestStripBrackets(t *testing.T) {
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	mk := func(sec float64, group int) Mark {
		return Mark{At: at.Add(time.Duration(sec * float64(time.Second))), Size: 100, Group: group}
	}
	h := &Head{From: at, To: at.Add(60 * time.Second), Marks: []Mark{
		mk(10, 1), mk(13, 2), mk(18, 1), mk(21, 2), // interleaved
		mk(30, 3), mk(34, 3), mk(42, 3), // a chain
		mk(50, 4), mk(50.5, 4), // a short gap
		mk(54, 5), mk(54.4, 5), mk(55.2, 6), mk(55.6, 6), // narrow ones close by: labels compete
	}}
	s := newStrip(h)
	row := map[int]float64{}
	for _, b := range s.Brackets {
		if b.Y > stripTop-8 {
			t.Errorf("bracket %+v reaches the marks (top %v)", b, stripTop)
		}
		row[b.group] = b.Y
	}
	if row[1] == row[2] {
		t.Error("interleaved retries share a row")
	}
	var chainRows []float64
	for _, b := range s.Brackets {
		if b.group == 3 {
			chainRows = append(chainRows, b.Y)
		}
	}
	if len(chainRows) != 2 || chainRows[0] != chainRows[1] {
		t.Errorf("a chain on rows %v", chainRows)
	}
	for i, a := range s.Brackets {
		if a.Label == "" {
			continue
		}
		aw := stripChar * float64(len(a.Label))
		if a.LX-aw/2 < 2 || a.LX+aw/2 > stripW-2 {
			t.Errorf("label %q off the strip", a.Label)
		}
		for _, b := range s.Brackets[i+1:] {
			if b.Label == "" || b.Y != a.Y {
				continue
			}
			bw := stripChar * float64(len(b.Label))
			if a.LX-aw/2 < b.LX+bw/2 && b.LX-bw/2 < a.LX+aw/2 {
				t.Errorf("labels %q and %q overlap", a.Label, b.Label)
			}
		}
	}
}
