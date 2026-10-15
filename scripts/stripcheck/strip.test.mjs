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
