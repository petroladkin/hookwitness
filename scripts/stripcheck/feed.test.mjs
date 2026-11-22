// Tests of what a closed live feed means (internal/web/static/feed.js),
// with Node's own test runner and no packages: make js-test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const src = readFileSync(new URL("../../internal/web/static/feed.js", import.meta.url), "utf8");
const sandbox = {};
vm.runInNewContext(src, sandbox);
const { closed } = sandbox.HookwitnessFeed;

// probe answers as fetch(location.href, {redirect: "manual"}) would.
const answer = (status, type = "basic") => () => Promise.resolve({ status, type });

test("a proxy failing the feed while the page loads: retry, never reload", async () => {
  for (const status of [200, 404, 500, 502, 503, 504]) {
    assert.equal(await closed(answer(status)), "retry", `page answered ${status}`);
  }
});

test("no answer at all (the server restarting, offline): retry", async () => {
  assert.equal(await closed(() => Promise.reject(new TypeError("Failed to fetch"))), "retry");
});

test("the login ended: reload onto the login page", async () => {
  assert.equal(await closed(answer(0, "opaqueredirect")), "reload", "a redirect, not followed");
  for (const status of [302, 303, 401, 403]) {
    assert.equal(await closed(answer(status)), "reload", `page answered ${status}`);
  }
});

test("an error, then the page fine again: still retry, the extension reopens the feed", async () => {
  const seq = [answer(502), answer(200)];
  for (const probe of seq) assert.equal(await closed(probe), "retry");
});
