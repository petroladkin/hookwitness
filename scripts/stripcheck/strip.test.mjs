// Tests of the strip's layout (internal/web/static/strip.js) with Node's
// own test runner and no packages: make js-test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const src = readFileSync(new URL("../../internal/web/static/strip.js", import.meta.url), "utf8");
const sandbox = {};
vm.runInNewContext(src, sandbox);
const { layout } = sandbox.HookwitnessStrip;

const measure = (s) => s.length * 6; // 10 px Martian Mono, about 6 px a character
const WINDOW = 120000;

// A strip of width w, the pen 28 px from the right, as app.js draws it.
function strip(w, h, deliveries, now = 200000) {
  const pen = w - 28;
  const x = (t) => pen - ((now - t) / WINDOW) * (pen - 8);
  const marks = deliveries.map(([at, group]) => ({ at, group, x: x(at) }));
  return { pen, ...layout(marks, w, h, pen, measure) };
}

function overlap(a, b) {
  return a[0] < b[1] && b[0] < a[1];
}

for (const [w, h] of [[358, 110], [1300, 150]]) {
  test(`labels stay on the paper and never overlap (${w} px)`, () => {
    // The docs' backoff: 503, 503 after 4 s, 200 after 8 s more; two bodies
    // sent twice 5 s apart; one retried across the left edge.
    const s = strip(w, h, [
      [76000, "old"], [83000, "old"], // starts 124 s ago: off the paper, still drawn (app.js keeps 5 s more)
      [130000, "a"], [134000, "a"], [142000, "a"],
      [150000, "b"], [155000, "b"],
      [190000, "c"], [195000, "c"],
    ]);
    const boxes = [];
    for (const br of s.brackets) {
      if (!br.label) continue;
      const wd = measure(br.label);
      const box = [br.lx - wd / 2, br.lx + wd / 2];
      assert.ok(box[0] >= 2 && box[1] <= s.pen - 4, `label ${br.label} off the paper: ${box}`);
      for (const [row, other] of boxes) if (row === br.row) assert.ok(!overlap(box, other), `labels overlap: ${br.label}`);
      boxes.push([br.row, box]);
    }
    assert.ok(boxes.length >= 3, "too few labels shown");
    const openLeft = s.brackets.find((br) => br.a < 0);
    assert.ok(openLeft && openLeft.openLeft, "a bracket from off the paper has no left tick");
  });

  test(`a label over its bracket fits between the ticks (${w} px)`, () => {
    // Retries 1 s apart: a bracket narrower than any label, which goes beside.
    const s = strip(w, h, [[180000, "a"], [181000, "a"], [190000, "b"], [196000, "b"]]);
    for (const br of s.brackets) {
      if (!br.label) continue;
      const wd = measure(br.label);
      const lo = Math.max(br.a, 0), hi = Math.min(br.b, s.pen);
      if (br.lx - wd / 2 < hi && br.lx + wd / 2 > lo) {
        assert.ok(br.lx - wd / 2 >= lo && br.lx + wd / 2 <= hi, `label ${br.label} over the ticks of ${lo}..${hi}`);
      }
    }
    if (w > 1000) assert.ok(s.brackets.some((br) => br.label && br.lx > br.b), "the narrow bracket's label is not beside it"); // on a phone it may not fit at all
  });

  test(`interleaved retries go on different rows (${w} px)`, () => {
    const s = strip(w, h, [[150000, "a"], [153000, "b"], [158000, "a"], [161000, "b"]]);
    const a = s.brackets.find((br) => br.group === "a");
    const b = s.brackets.find((br) => br.group === "b");
    assert.notEqual(a.row, b.row);
    assert.notEqual(a.y, b.y);
  });

  test(`a chain of retries shares a row (${w} px)`, () => {
    const s = strip(w, h, [[150000, "a"], [154000, "a"], [162000, "a"]]);
    assert.equal(new Set(s.brackets.map((br) => br.row)).size, 1);
  });

  test(`brackets stay clear of the marks (${w} px)`, () => {
    const s = strip(w, h, [[150000, "a"], [153000, "b"], [158000, "a"], [161000, "b"], [170000, "c"], [171000, "c"]]);
    for (const br of s.brackets) assert.ok(br.y + 6 <= s.top - 8, `tick at ${br.y + 6}, marks from ${s.top}`);
    for (const br of s.brackets) if (br.label) assert.ok(br.y - 4 - 8 >= 0, `label above the canvas: ${br.y}`);
  });

  test(`"now" is clear of the pen's arrow (${w} px)`, () => {
    const s = strip(w, h, []);
    const box = [s.now.x - measure("now"), s.now.x];
    assert.equal(s.now.align, "right");
    assert.ok(box[0] > s.pen + 6 && box[1] <= w, `now at ${box}, arrow to ${s.pen + 6}`);
  });
}

const { SPANS, ago, full, short } = sandbox.HookwitnessStrip;

test("the ranges: 2 min, 10 min, 1 h, 6 h, each with its grid and labels", () => {
  assert.deepEqual(Object.keys(SPANS), ["2m", "10m", "1h", "6h"]);
  const want = {
    "2m": [120000, 2000, 10000, 30000, ["−30 s", "−60 s", "−90 s"]],
    "10m": [600000, 10000, 60000, 120000, ["−2 min", "−4 min", "−6 min", "−8 min"]],
    "1h": [3600000, 60000, 600000, 600000, ["−10 min", "−20 min", "−30 min", "−40 min", "−50 min"]],
    "6h": [21600000, 600000, 3600000, 3600000, ["−1 h", "−2 h", "−3 h", "−4 h", "−5 h"]],
  };
  for (const [k, [span, line, major, label, labels]] of Object.entries(want)) {
    const s = SPANS[k];
    assert.deepEqual([s.span, s.line, s.major, s.label], [span, line, major, label], k);
    const got = [];
    for (let t = s.label; t < s.span; t += s.label) got.push(ago(t, s.span));
    assert.deepEqual(got, labels, k);
    assert.equal(s.major % s.line, 0, `${k}: major lines fall on lines`);
    assert.ok(s.span / s.line <= 60, `${k}: at most 60 lines across`);
  }
});

test("gaps between deliveries in their own unit", () => {
  for (const [ms, f, sh] of [
    [4000, "+4.0 s", "4.0s"], [45000, "+45.0 s", "45s"],
    [300000, "+5 min", "5m"], [330000, "+5.5 min", "5.5m"], [1800000, "+30 min", "30m"],
    [7200000, "+2 h", "2h"], [18000000, "+5 h", "5h"], [9000000, "+2.5 h", "2.5h"],
  ]) {
    assert.equal(full(ms), f);
    assert.equal(short(ms), sh);
  }
});

test("a long range's retries get labels in minutes and hours", () => {
  // Standard Webhooks' schedule on the 6 h paper: 5 s, 5 min, 30 min, 2 h.
  const w = 1300, pen = w - 28, span = SPANS["6h"].span, now = 30000000;
  const x = (t) => pen - ((now - t) / span) * (pen - 8);
  let at = now - 3 * 3600000;
  const marks = [];
  for (const gap of [0, 5000, 300000, 1800000, 7200000]) {
    at += gap;
    marks.push({ at, group: "a", x: x(at) });
  }
  const labels = layout(marks, w, 150, pen, measure).brackets.map((b) => b.label).filter(Boolean);
  assert.ok(labels.includes("+2 h") && labels.includes("+30 min"), `labels: ${labels}`);
});
