// Screenshots for the documentation, from a seeded instance: starts the
// binary on a temporary database, fills it through the REST API with
// synthetic webhooks, and captures pages through the Chrome DevTools
// Protocol (light theme, 1280 px wide, 2x pixels, WebP). No npm packages.
//
// Usage: node scripts/media/media.mjs <hookwitness binary> <out dir> [name...]
// Needs Node 22+ and Chrome or Chromium (CHROME=<path> to choose one).
// With names, only those shots are taken. `make media` runs it.
import { spawn, execFileSync } from "node:child_process";
import { createHmac } from "node:crypto";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { request } from "node:http";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const [bin, outDir, ...only] = process.argv.slice(2);
if (!bin || !outDir) {
  console.error("usage: media.mjs <hookwitness binary> <out dir> [name...]");
  process.exit(2);
}
mkdirSync(outDir, { recursive: true });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const work = mkdtempSync(join(tmpdir(), "hookwitness-media-"));
const children = [];
process.on("exit", () => {
  for (const c of children) c.kill();
  try {
    rmSync(work, { recursive: true, force: true, maxRetries: 5 });
  } catch {} // Chrome may still be writing its profile; the OS cleans tmp
});

function freePort() {
  return new Promise((ok) => {
    const s = createServer().listen(0, "127.0.0.1", () => {
      const { port } = s.address();
      s.close(() => ok(port));
    });
  });
}

// --- the instance ---------------------------------------------------------

// The documentation's own address when it is free, so the catch URLs in
// the shots read as in the text.
const port = await new Promise((ok) => {
  const s = createServer().once("error", () => ok(freePort())).listen(8080, "127.0.0.1", () => s.close(() => ok(8080)));
});
children.push(spawn(resolve(bin), ["serve", "-addr", `127.0.0.1:${port}`, "-db", join(work, "hw.db"),
  "-admin-hosts", "localhost"], { stdio: "ignore",
  // A fixed test key: secrets' fingerprints in the shots stay the same.
  env: { ...process.env, HOOKWITNESS_SECRET_KEY: "bWVkaWEtc2NyaXB0LXRlc3Qta2V5LTMyLWJ5dGVzISE=" } }));
const HW = `http://127.0.0.1:${port}`;
const API = `${HW}/admin/api/v1`;
for (let i = 0; ; i++) {
  try { if ((await fetch(`${HW}/healthz`)).ok) break; } catch {}
  if (i > 80) throw new Error("hookwitness did not start");
  await sleep(100);
}

async function api(method, path, body, headers = {}) {
  const res = await fetch(API + path, {
    method, headers: { "Content-Type": "application/json", ...headers },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  if (!res.ok) throw new Error(`${method} ${path}: ${res.status} ${text}`);
  return { json: text ? JSON.parse(text) : null, etag: res.headers.get("etag") };
}
async function behave(id, change) {
  const { json, etag } = await api("GET", `/endpoints/${id}/behaviour`);
  delete json.secrets;
  await api("PUT", `/endpoints/${id}/behaviour`, { ...json, ...change }, { "If-Match": etag });
}
// send posts like a webhook sender: only the headers given (fetch would add
// browser ones). Timeouts and drops are part of the scenario.
async function send(path, body, headers = {}) {
  await new Promise((done) => {
    const req = request(HW + path, { method: "POST", timeout: 1500, headers: {
      "Content-Type": "application/json", "User-Agent": "shop-backend/2.3", ...headers,
      "Content-Length": Buffer.byteLength(body) } }, (res) => { res.resume(); res.on("end", done); });
    req.on("timeout", () => req.destroy());
    req.on("error", done);
    req.end(body);
  });
  await sleep(40); // distinct arrival times on the strip
}
const hmac = (key, s) => createHmac("sha256", key).update(s).digest("hex");

// A GitHub-style endpoint: signed deliveries, every 4th answered 503.
const gh = (await api("POST", "/endpoints", { path: "/github/webhooks" })).json.id;
await api("PUT", `/endpoints/${gh}/secrets/GH_SECRET`, { kind: "key", value: "gh_test_secret" });
await behave(gh, {
  checks: [{ kind: "github", secret: "GH_SECRET" }],
  rules: [{ when: { kind: "every", n: 4 }, do: "reply", reply: { status: 503, headers: [{ name: "Retry-After", value: "2" }] } }],
});
const events = [
  { action: "opened", number: 7, pull_request: { title: "Add retries to the exporter", user: { login: "octo-dev" } } },
  { action: "synchronize", number: 7, before: "9f1c2e4", after: "4b8d0a7" },
  { action: "created", issue: { number: 12 }, comment: { body: "Looks good to me" } },
  { action: "closed", number: 7, pull_request: { merged: true } },
  { action: "opened", number: 8, pull_request: { title: "Bump the SDK", user: { login: "dependabot" } } },
  { action: "labeled", number: 8, label: { name: "dependencies" } },
];
for (const [i, e] of events.entries()) {
  const body = JSON.stringify(e);
  const sig = i === 4 ? hmac("old_secret", body) : hmac("gh_test_secret", body);
  await send("/github/webhooks", body, {
    "X-GitHub-Event": i === 2 ? "issue_comment" : "pull_request",
    "X-GitHub-Delivery": `7f0d6b2e-1c3a-4e8f-9b5d-00000000000${i}`,
    "X-Hub-Signature-256": `sha256=${sig}`, "User-Agent": "GitHub-Hookshot/5c3e1b2",
  });
}

// A shop endpoint: JSON Schema picked by /type, an image in the payload.
const shop = (await api("POST", "/endpoints", { path: "/shop/events" })).json.id;
const orderPaid = {
  type: "object", required: ["type", "order_id", "amount"],
  properties: {
    type: { const: "order.paid" }, order_id: { type: "integer" },
    amount: { type: "integer", minimum: 1 }, paid_at: { type: "string", format: "date-time" },
  },
};
const receipt = {
  type: "object", required: ["type", "order_id", "scan"],
  properties: {
    type: { const: "receipt.scanned" }, order_id: { type: "integer" },
    scan: { type: "string", contentEncoding: "base64", contentMediaType: "image/png" },
  },
};
await behave(shop, {
  schemas: [{ name: "OrderPaid", text: JSON.stringify(orderPaid, null, 2) },
    { name: "ReceiptScanned", text: JSON.stringify(receipt, null, 2) }],
  checks: [{ kind: "schema", select: { from: "body", key: "/type",
    map: [{ value: "order.paid", schema: "OrderPaid" }, { value: "receipt.scanned", schema: "ReceiptScanned" }] } }],
});
const png = readFileSync(new URL("receipt.png", import.meta.url)).toString("base64");
for (const b of [
  { type: "order.paid", order_id: 42, amount: 1999, paid_at: "2026-10-12T17:28:07Z" },
  { type: "order.paid", order_id: "43", paid_at: "yesterday" },
  { type: "order.shipped", order_id: 42, carrier: "DHL", shipped_at: "2026-10-13T08:02:51Z", items: [{ sku: "A-1", qty: 2 }] },
  { type: "order.shipped", order_id: 44, carrier: "UPS", shipped_at: "2026-10-13T09:40:31Z", items: [{ sku: "C-2", qty: 1 }], tracking: "https://track.example.com/1Z999" },
  { type: "receipt.scanned", order_id: 42, scan: png },
]) await send("/shop/events", JSON.stringify(b));

// A slow and flaky endpoint for the behaviour pages.
const flaky = (await api("POST", "/endpoints", { path: "/billing/notify" })).json.id;
await behave(flaky, {
  default: { status: 200, headers: [{ name: "Content-Type", value: "application/json" }], body: '{"received":true,"n":{{.N}}}', delay_ms: 300 },
  rules: [
    { when: { kind: "first", n: 2 }, do: "reply", reply: { status: 503, headers: [{ name: "Retry-After", value: "1" }] } },
    { when: { kind: "every", n: 5 }, do: "drop" },
  ],
  checks: [{ header: "X-Api-Key", match: "equals", value: "key_test" }],
  vars: [{ name: "TOKEN", value: "tok_test" }],
});
for (let i = 1; i <= 7; i++) await send("/billing/notify", JSON.stringify({ invoice: `in_${1000 + i}`, status: "paid" }), { "X-Api-Key": i === 6 ? "wrong" : "key_test" });

// A typo in a sender's URL.
await send("/github/webhook", JSON.stringify({ action: "opened" }));

// --- the browser -----------------------------------------------------------

const chromePath = process.env.CHROME || [
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
  "/Applications/Chromium.app/Contents/MacOS/Chromium",
  "/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser",
].find(existsSync);
if (!chromePath) throw new Error("Chrome not found: set CHROME=<path>");
const cdpPort = await freePort();
children.push(spawn(chromePath, ["--headless=new", `--remote-debugging-port=${cdpPort}`,
  `--user-data-dir=${join(work, "chrome")}`, "--disable-gpu", "--hide-scrollbars", "--no-first-run",
  "--host-resolver-rules=MAP hooks.example.com 127.0.0.1", "about:blank"], { stdio: "ignore" }));
let wsURL;
for (let i = 0; !wsURL; i++) {
  try {
    const list = await (await fetch(`http://127.0.0.1:${cdpPort}/json/list`)).json();
    wsURL = list.find((t) => t.type === "page")?.webSocketDebuggerUrl;
  } catch {}
  if (i > 100) throw new Error("Chrome did not start");
  if (!wsURL) await sleep(100);
}
const ws = new WebSocket(wsURL);
await new Promise((r) => ws.addEventListener("open", r, { once: true }));
let seq = 0;
const pending = new Map();
ws.addEventListener("message", (m) => {
  const msg = JSON.parse(m.data);
  if (msg.id && pending.has(msg.id)) {
    pending.get(msg.id)(msg);
    pending.delete(msg.id);
  }
});
const cdp = (method, params = {}) => new Promise((ok, fail) => {
  const id = ++seq;
  pending.set(id, (msg) => (msg.error ? fail(new Error(`${method}: ${msg.error.message}`)) : ok(msg.result)));
  ws.send(JSON.stringify({ id, method, params }));
});
const js = async (expression) => (await cdp("Runtime.evaluate", { expression, awaitPromise: true })).result.value;
await cdp("Page.enable");
await cdp("Emulation.setDeviceMetricsOverride", { width: 1280, height: 800, deviceScaleFactor: 2, mobile: false });
await cdp("Emulation.setEmulatedMedia", { media: "", features: [{ name: "prefers-color-scheme", value: "light" }] });

// shot navigates, optionally runs a step (a click, a choice), and captures
// the page down to its last element, at most maxHeight CSS pixels; with a
// selector, only that element (a dialog) with a margin.
async function shot(name, url, { step, maxHeight = 800, selector } = {}) {
  if (only.length && !only.includes(name)) return;
  await cdp("Page.navigate", { url });
  await sleep(900);
  if (step) {
    await js(step);
    await sleep(500);
  }
  const bottom = await js(`Math.ceil(Math.max(...[...document.body.querySelectorAll("*")]
    .filter((e) => !e.children.length && e.offsetParent && !e.closest("dialog:not([open])"))
    .map((e) => e.getBoundingClientRect().bottom + scrollY)) + 24)`);
  let clip = { x: 0, y: 0, width: 1280, height: Math.min(bottom, maxHeight), scale: 1 };
  if (selector) {
    const r = JSON.parse(await js(`(() => { const b = document.querySelector(${JSON.stringify(selector)}).getBoundingClientRect();
      return JSON.stringify({ x: b.x, y: b.y + scrollY, width: b.width, height: b.height }); })()`));
    clip = { x: Math.max(0, r.x - 16), y: Math.max(0, r.y - 16), width: Math.min(1280, r.width + 32), height: Math.min(r.height + 32, maxHeight), scale: 1 };
  }
  const { data } = await cdp("Page.captureScreenshot", { format: "webp", quality: 88, captureBeyondViewport: true, clip });
  const file = join(outDir, `${name}.webp`);
  writeFileSync(file, Buffer.from(data, "base64"));
  console.log(`${file}  ${Math.round(Buffer.byteLength(data, "base64") / 1024)} KiB`);
}
const UI = `${HW}/admin`;
const reqs = async (id) => (await api("GET", `/endpoints/${id}/requests?limit=50`)).json.requests;
const ghReqs = await reqs(gh);
const shopReqs = await reqs(shop);
const byNo = (list, no) => list.find((r) => r.no === no).id;

const choose = (name, value) => `(() => { const e = document.querySelector('select[name=${name}]');
  e.value = ${JSON.stringify(value)}; e.dispatchEvent(new Event("change", { bubbles: true })); })()`;
const dialog = "dialog[open]";

await shot("endpoints", `${UI}/`);
await shot("new-endpoint", `${UI}/?edit=new`, { selector: dialog });
await shot("register", `${UI}/e/${gh}`);
await shot("request", `${UI}/e/${gh}/r/${byNo(ghReqs, 5)}`, { maxHeight: 1100 });
await shot("unmatched", `${UI}/e/unmatched`, { maxHeight: 600 });
await shot("behaviour-rules", `${UI}/e/${flaky}/behaviour?tab=rules`);
await shot("behaviour-rule-edit", `${UI}/e/${flaky}/behaviour?tab=rules&edit=rule-0`, { selector: dialog });
await shot("behaviour-default", `${UI}/e/${flaky}/behaviour?tab=default`, { maxHeight: 1000 });
await shot("checks", `${UI}/e/${gh}/behaviour?tab=checks`);
await shot("check-new", `${UI}/e/${gh}/behaviour?tab=checks&edit=check-new`, { step: choose("kind", "github"), selector: dialog });
await shot("secrets", `${UI}/e/${gh}/behaviour?tab=secrets`);
await shot("secret-new", `${UI}/e/${gh}/behaviour?tab=secrets&edit=secret-new`, { selector: dialog });
await shot("schemas", `${UI}/e/${shop}/behaviour?tab=schemas`);
await shot("schema-check", `${UI}/e/${shop}/behaviour?tab=checks&edit=check-0`, { selector: dialog, maxHeight: 1400 });
await shot("schema-fail", `${UI}/e/${shop}/r/${byNo(shopReqs, 2)}`, { maxHeight: 1100 });
await shot("draft-dialog", `${UI}/e/${shop}?draft&value=order.shipped`, { selector: dialog });
await shot("draft-editor", `${UI}/e/${shop}/behaviour?tab=schemas&edit=schema-draft&by=value&value=order.shipped`, { maxHeight: 1100 });
await shot("payload-images", `${UI}/e/${shop}/r/${byNo(shopReqs, 5)}`, { maxHeight: 900 });
await shot("report-dialog", `${UI}/e/${gh}?report`, { selector: dialog, maxHeight: 1400 });
if (!only.length || only.includes("report-html")) {
  const res = await fetch(`${API}/endpoints/${gh}/report?format=html`);
  writeFileSync(join(work, "report.html"), await res.text());
}
await shot("report-html", `file://${join(work, "report.html")}`, { maxHeight: 1000 });
await shot("host-refused", `http://hooks.example.com:${port}/admin/`, { maxHeight: 80 });
ws.close();
process.exit(0);
