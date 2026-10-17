// The strip's layout, kept apart from drawing so it can be tested without a
// browser (scripts/stripcheck): where the brackets between deliveries of
// the same body go, on which row, and where their gap labels fit.
(function (root) {
  "use strict";

  const ROW = 13; // pixels between bracket rows
  const TICK = 6; // a bracket's end ticks
  const GAP = 3; // free pixels around a label

  // rowsFor is how many bracket rows a strip of height h has room for.
  function rowsFor(h) {
    return h >= 140 ? 3 : 2;
  }

  // top is the highest a mark reaches: below every bracket row and its
  // labels, so a timeout's circle there never meets a bracket's tick.
  function top(rows) {
    return 27 + ROW * (rows - 1);
  }

  // rowY is the bracket line of row r; its ticks end 8 px above top.
  function rowY(rows, r) {
    return top(rows) - 14 - ROW * r;
  }

  function full(gapMs) {
    return "+" + (gapMs / 1000).toFixed(1) + " s";
  }
  function short(gapMs) {
    const sec = gapMs / 1000;
    return (sec < 10 ? sec.toFixed(1) : String(Math.round(sec))) + "s"; // a tenth kept where it matters
  }

  // layout places the brackets of marks (sorted by time, each {x, group,
  // at}) on a strip of width w and height h with the pen at pen; measure
  // gives a label's width in pixels. It returns the top of the marks, the
  // brackets ({a, b, row, y, openLeft, label, lx}; label "" when none fits)
  // and where "now" is written.
  function layout(marks, w, h, pen, measure) {
    const rows = rowsFor(h);
    const segs = [];
    const prev = {};
    marks.forEach((m) => {
      if (!m.group) return;
      const p = prev[m.group];
      prev[m.group] = m;
      if (p && m.x >= 0) segs.push({ a: p.x, b: m.x, group: m.group, gap: m.at - p.at });
    });
    segs.sort((s, t) => s.a - t.a || s.b - t.b);

    // Rows: the lowest whose last bracket ended before this one starts, or
    // that holds the same body's previous bracket ending here (a chain of
    // retries shares its ticks); when none is free, the least crowded.
    const ends = [], groups = [];
    for (let r = 0; r < rows; r++) {
      ends.push(-Infinity);
      groups.push("");
    }
    segs.forEach((s) => {
      let row = -1;
      for (let r = 0; r < rows && row < 0; r++) {
        if (ends[r] < s.a - TICK || (groups[r] === s.group && ends[r] === s.a)) row = r;
      }
      if (row < 0) {
        row = 0;
        for (let r = 1; r < rows; r++) if (ends[r] < ends[row]) row = r;
        s.crowded = true;
      }
      ends[row] = Math.max(ends[row], s.b);
      groups[row] = s.group;
      s.row = row;
      s.y = rowY(rows, row);
      s.openLeft = s.a < 0;
    });

    // Labels, newest first: the full gap inside the bracket, the short one
    // inside it, then either beside it; never over another label of its
    // row, never off the paper. A label that fits nowhere is left out: the
    // register's row says "retry N" anyway.
    const taken = [];
    for (let r = 0; r < rows; r++) taken.push([]);
    const free = (row, l, rgt) => l >= 2 && rgt <= pen - 4 && taken[row].every(([x0, x1]) => rgt + GAP <= x0 || l >= x1 + GAP);
    segs
      .slice()
      .sort((s, t) => t.b - s.b)
      .forEach((s) => {
        s.label = "";
        const lo = Math.max(s.a, 0), hi = Math.min(s.b, pen);
        const tries = [];
        [full(s.gap), short(s.gap)].forEach((text) => {
          const wd = measure(text);
          tries.push({ text, l: (lo + hi) / 2 - wd / 2, wd, inside: true });
        });
        [full(s.gap), short(s.gap)].forEach((text) => {
          const wd = measure(text);
          tries.push({ text, l: hi + GAP, wd }, { text, l: lo - GAP - wd, wd });
        });
        for (const c of tries) {
          if (c.inside && c.wd + 2 * GAP > hi - lo) continue;
          if (!free(s.row, c.l, c.l + c.wd)) continue;
          s.label = c.text;
          s.lx = c.l + c.wd / 2;
          taken[s.row].push([c.l, c.l + c.wd]);
          break;
        }
      });

    return { top: top(rows), rows, brackets: segs, now: { x: w - 2, align: "right" } };
  }

  root.HookwitnessStrip = { layout, rowsFor, top, rowY };
})(typeof window !== "undefined" ? window : globalThis);
