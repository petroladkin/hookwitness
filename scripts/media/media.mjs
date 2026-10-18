// Screenshots for the documentation, from a seeded instance: starts the
// binary on a temporary database, fills it through the REST API with
// synthetic webhooks, and captures pages through the Chrome DevTools
// Protocol (light theme, 1280 px wide, 2x pixels, WebP). No npm packages.
//
// Usage: node scripts/media/media.mjs <hookwitness binary> <out dir> [name...]
// Needs Node 22+, Chrome or Chromium (CHROME=<path> to choose one) and, for
// the README's demo GIF, Docker (ffmpeg runs in a container). The social
// preview goes to <out dir>/../media.
// With names, only those shots are taken. `make media` runs it.
// MEDIA_WIDTH (default 1280) and MEDIA_SCHEME (light or dark) take the same
// shots at another width or in the other theme, for evidence elsewhere.
import { spawn, execFileSync } from "node:child_process";
import { createHmac } from "node:crypto";
import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
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
const WIDTH = Number(process.env.MEDIA_WIDTH) || 1280;
await cdp("Emulation.setDeviceMetricsOverride", { width: WIDTH, height: 800, deviceScaleFactor: 2, mobile: WIDTH < 600 });
await cdp("Emulation.setEmulatedMedia", { media: "", features: [{ name: "prefers-color-scheme", value: process.env.MEDIA_SCHEME || "light" }] });

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
  let clip = { x: 0, y: 0, width: WIDTH, height: Math.min(bottom, maxHeight), scale: 1 };
  if (selector) {
    const r = JSON.parse(await js(`(() => { const b = document.querySelector(${JSON.stringify(selector)}).getBoundingClientRect();
      return JSON.stringify({ x: b.x, y: b.y + scrollY, width: b.width, height: b.height }); })()`));
    clip = { x: Math.max(0, r.x - 16), y: Math.max(0, r.y - 16), width: Math.min(WIDTH, r.width + 32), height: Math.min(r.height + 32, maxHeight), scale: 1 };
  }
  const { data } = await cdp("Page.captureScreenshot", { format: "webp", quality: 88, captureBeyondViewport: true, clip });
  const file = join(outDir, `${name}.webp`);
  writeFileSync(file, Buffer.from(data, "base64"));
  console.log(`${file}  ${Math.round(Buffer.byteLength(data, "base64") / 1024)} KiB`);
}
const UI = `${HW}/admin`;
const reqs = async (id) => (await api("GET", `/endpoints/${id}/requests?limit=50`)).json.requests;
const byNo = async (id, no) => (await reqs(id)).find((r) => r.no === no).id;
const choose = (name, value) => `(() => { const e = document.querySelector('dialog[open] select[name=${name}]');
  e.value = ${JSON.stringify(value)}; e.dispatchEvent(new Event("change", { bubbles: true })); })()`;
const type = (name, value) => `(() => { const e = document.querySelector('dialog[open] [name=${name}]');
  e.value = ${JSON.stringify(value)}; e.dispatchEvent(new Event("input", { bubbles: true })); })()`;
const steps = (...xs) => xs.join(";\n");
const dialog = "dialog[open]";

// Each part below does what its documentation page tells the reader to do,
// with the same paths, values and requests, then shoots it.

// docs/getting-started.md: an empty server, /stripe/webhooks created, one
// invoice.paid event sent with curl.
await shot("new-endpoint", `${UI}/?edit=new`, { step: type("path", "/stripe/webhooks"), selector: dialog });
const stripe = (await api("POST", "/endpoints", { path: "/stripe/webhooks" })).json.id;
await shot("endpoints", `${UI}/`);
await send("/stripe/webhooks", JSON.stringify({ type: "invoice.paid", id: "evt_1" }), { "User-Agent": "curl/8.7.1" });
await shot("first-request", `${UI}/e/${stripe}/r/${await byNo(stripe, 1)}`, { maxHeight: 1000 });

// docs/endpoints.md: the sender's typo, and a register with retries, a rule
// and verdicts (GitHub deliveries, one signed with an old secret, every 4th
// answered 503).
await send("/stripe/webhook", "{}", { "User-Agent": "curl/8.7.1" });
await shot("unmatched", `${UI}/e/unmatched`, { maxHeight: 600 });
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
  await send("/github/webhooks", body, {
    "X-GitHub-Event": i === 2 ? "issue_comment" : "pull_request",
    "X-GitHub-Delivery": `7f0d6b2e-1c3a-4e8f-9b5d-00000000000${i}`,
    "X-Hub-Signature-256": `sha256=${hmac(i === 4 ? "old_secret" : "gh_test_secret", body)}`,
    "User-Agent": "GitHub-Hookshot/5c3e1b2",
  });
}
await shot("register", `${UI}/e/${gh}`);

// docs/replies.md: /billing/notify fails every 3rd delivery, /billing/retry
// fails the first two with Retry-After: 2, /slack/events echoes the
// challenge.
const billing = (await api("POST", "/endpoints", { path: "/billing/notify" })).json.id;
await behave(billing, { rules: [{ when: { kind: "every", n: 3 }, do: "reply", reply: { status: 500 } }] });
for (let i = 1; i <= 3; i++) await send("/billing/notify", JSON.stringify({ invoice: `in_${i}` }), { "User-Agent": "curl/8.7.1" });
await shot("behaviour-rules", `${UI}/e/${billing}/behaviour?tab=rules`);
const retry = (await api("POST", "/endpoints", { path: "/billing/retry" })).json.id;
await behave(retry, {
  rules: [{ when: { kind: "first", n: 2 }, do: "reply", reply: { status: 503, headers: [{ name: "Retry-After", value: "2" }] } }],
});
await shot("behaviour-rule-edit", `${UI}/e/${retry}/behaviour?tab=rules&edit=rule-0`, { selector: dialog });
const slack = (await api("POST", "/endpoints", { path: "/slack/events" })).json.id;
await behave(slack, {
  default: { status: 200, headers: [{ name: "Content-Type", value: "application/json" }], body: '{"challenge":"{{.JSON "challenge"}}"}' },
});
await shot("behaviour-default", `${UI}/e/${slack}/behaviour?tab=default`, { maxHeight: 1000 });

// docs/authorization.md: protect /hooks with the token API_TOKEN,
// step by step, then the three requests of its example.
const hooks = (await api("POST", "/endpoints", { path: "/hooks" })).json.id;
await shot("secret-new", `${UI}/e/${hooks}/behaviour?tab=secrets&edit=secret-new`, {
  step: steps(type("name", "API_TOKEN"), choose("secret_kind", "token"), type("value", "tok_test_123")), selector: dialog });
await api("PUT", `/endpoints/${hooks}/secrets/API_TOKEN`, { kind: "token", value: "tok_test_123" });
await shot("check-new", `${UI}/e/${hooks}/behaviour?tab=checks&edit=check-new`, {
  step: steps(choose("kind", "bearer"), choose("secret", "API_TOKEN")), selector: dialog });
await behave(hooks, { checks: [{ kind: "bearer", secret: "API_TOKEN" }] });
await shot("checks", `${UI}/e/${hooks}/behaviour?tab=checks`);
await shot("secrets", `${UI}/e/${hooks}/behaviour?tab=secrets`);
for (const auth of ["Bearer tok_test_123", "Bearer wrong", null]) {
  await send("/hooks", JSON.stringify({ order: 42 }), { "User-Agent": "curl/8.7.1", ...(auth ? { Authorization: auth } : {}) });
}
await shot("auth-fail", `${UI}/e/${hooks}/r/${await byNo(hooks, 2)}`, { maxHeight: 1000 });

// docs/schemas.md: /orders/events with the page's OrderPaid schema picked by
// /type, the three requests of its example, two shipped orders to draft
// from, and a receipt whose scan is a declared image.
const orders = (await api("POST", "/endpoints", { path: "/orders/events" })).json.id;
const orderPaid = '{"type":"object","required":["order_id","amount"],\n "properties":{"order_id":{"type":"integer"},"amount":{"type":"integer","minimum":1},\n               "paid_at":{"type":"string","format":"date-time"}}}';
const receipt = {
  type: "object", required: ["order_id", "scan"],
  properties: { order_id: { type: "integer" }, scan: { type: "string", contentEncoding: "base64", contentMediaType: "image/png" } },
};
await behave(orders, {
  schemas: [{ name: "OrderPaid", text: orderPaid }, { name: "ReceiptScanned", text: JSON.stringify(receipt, null, 2) }],
  checks: [{ kind: "schema", select: { from: "body", key: "/type",
    map: [{ value: "order.paid", schema: "OrderPaid" }, { value: "receipt.scanned", schema: "ReceiptScanned" }] } }],
});
const png = readFileSync(new URL("receipt.png", import.meta.url)).toString("base64");
for (const b of [
  '{"type":"order.paid","order_id":42,"amount":1999,"paid_at":"2026-10-12T17:28:07Z"}',
  '{"type":"order.paid","order_id":"42","paid_at":"yesterday"}',
  '{"type":"order.created","order_id":43}',
  JSON.stringify({ type: "order.shipped", order_id: 42, carrier: "DHL", shipped_at: "2026-10-13T08:02:51Z", items: [{ sku: "A-1", qty: 2 }] }),
  JSON.stringify({ type: "order.shipped", order_id: 44, carrier: "UPS", shipped_at: "2026-10-13T09:40:31Z", items: [{ sku: "C-2", qty: 1 }], tracking: "https://track.example.com/1Z999" }),
  JSON.stringify({ type: "receipt.scanned", order_id: 42, scan: png }),
]) await send("/orders/events", b, { "User-Agent": "curl/8.7.1" });
await shot("schemas", `${UI}/e/${orders}/behaviour?tab=schemas`);
await shot("schema-check", `${UI}/e/${orders}/behaviour?tab=checks&edit=check-0`, { selector: dialog, maxHeight: 1400 });
await shot("schema-fail", `${UI}/e/${orders}/r/${await byNo(orders, 2)}`, { maxHeight: 1100 });
await shot("draft-dialog", `${UI}/e/${orders}?draft&value=order.shipped`, { selector: dialog });
await shot("draft-editor", `${UI}/e/${orders}/behaviour?tab=schemas&edit=schema-draft&by=value&value=order.shipped`, { maxHeight: 1100 });
await shot("payload-images", `${UI}/e/${orders}/r/${await byNo(orders, 6)}`, { maxHeight: 900 });

// docs/reports.md: the report of the GitHub register above.
await shot("report-dialog", `${UI}/e/${gh}?report`, { selector: dialog, maxHeight: 1400 });
if (!only.length || only.includes("report-html")) {
  const res = await fetch(`${API}/endpoints/${gh}/report?format=html`);
  writeFileSync(join(work, "report.html"), await res.text());
}
await shot("report-html", `file://${join(work, "report.html")}`, { maxHeight: 1000 });

// docs/endpoints.md, "The strip": a sender's real minute on its own
// endpoint, so every mark the text names is on the paper: a body retried
// with backoff (4 s, then 8 s) after two 503s, a burst, a failed check, a timeout, a drop,
// a large body, the same body delivered twice five seconds apart. It runs in real time
// (about 70 s), last, so the other shots do not wait for it.
if (!only.length || only.includes("strip")) {
  const pay = (await api("POST", "/endpoints", { path: "/payments/webhooks" })).json.id;
  await api("PUT", `/endpoints/${pay}/secrets/PAY_TOKEN`, { kind: "token", value: "tok_pay_test" });
  await behave(pay, { checks: [{ kind: "bearer", secret: "PAY_TOKEN" }] });
  const start = Date.now();
  const at = async (sec) => { const d = start + sec * 1000 - Date.now(); if (d > 0) await sleep(d); };
  const auth = { Authorization: "Bearer tok_pay_test", "User-Agent": "payments/4.1" };
  const event = (id, extra = {}) => JSON.stringify({ id: `evt_${id}`, type: "payment.succeeded", amount: 1999, currency: "EUR", ...extra });
  const big = event("refund_batch", { items: Array.from({ length: 400 }, (_, i) => ({ id: `re_${i}`, amount: 100 + i })) });
  const a = event("1001", { customer: { id: "cus_42", email: "buyer@example.com" }, lines: Array.from({ length: 30 }, (_, i) => ({ sku: `SKU-${i}`, qty: 1 })) });
  for (const [i, t] of [0, 0.3, 0.5, 0.9, 1.2].entries()) { await at(t); await send("/payments/webhooks", event(`20${i}`), auth); }
  await at(8); await send("/payments/webhooks", event("3001"), { ...auth, Authorization: "Bearer revoked" });
  await behave(pay, { rules: [{ when: { kind: "every", n: 1 }, do: "timeout" }] });
  await at(16); await send("/payments/webhooks", event("4001"), auth);  // held until the sender gives up
  await behave(pay, { rules: [{ when: { kind: "every", n: 1 }, do: "drop" }] });
  await at(24); await send("/payments/webhooks", event("5001"), auth);
  await behave(pay, { rules: [] });
  await at(32); await send("/payments/webhooks", big, auth);
  await behave(pay, { rules: [{ when: { kind: "first", n: 2 }, do: "reply", reply: { status: 503, headers: [{ name: "Retry-After", value: "4" }] } }] });
  await api("POST", `/endpoints/${pay}/reset-counter`, {});
  await at(54); await send("/payments/webhooks", a, auth);         // 503
  await at(58); await send("/payments/webhooks", a, auth);         // 503, retried after 4 s
  await at(66); await send("/payments/webhooks", a, auth);         // 200, after 8 s more
  const b = event("6001", { note: "sent twice" });
  await at(67); await send("/payments/webhooks", b, auth);
  await at(72); await send("/payments/webhooks", b, auth);         // the same body again
  await at(73);
  await shot("strip", `${UI}/e/${pay}`, { selector: "section.strip", step: "new Promise((r) => setTimeout(r, 300))" });
}

// README.md, the demo: a GIF of one real run. Webhooks arrive live, every
// 3rd is answered 503 by a rule, a badly signed one turns red and its page
// gives the reason, and it ends with the report downloaded and opened.
// Frames come from the DevTools screencast; ffmpeg in a container encodes
// them (a palette per GIF, frame times kept).
const FFMPEG = "linuxserver/ffmpeg:7.1.1@sha256:aea59a11c54291ac456bb2d67000445e5a8994f70bc3d96cdc29f022fbbf89fb";
if (!only.length || only.includes("demo")) {
  const ev = (await api("POST", "/endpoints", { path: "/github/events" })).json.id;
  await api("PUT", `/endpoints/${ev}/secrets/GH_SECRET`, { kind: "key", value: "gh_test_secret" });
  await behave(ev, {
    checks: [{ kind: "github", secret: "GH_SECRET" }],
    rules: [{ when: { kind: "every", n: 3 }, do: "reply", reply: { status: 503 } }],
  });
  const deliver = (e, key = "gh_test_secret") => {
    const body = JSON.stringify(e);
    return send("/github/events", body, {
      "X-GitHub-Event": "pull_request", "X-Hub-Signature-256": `sha256=${hmac(key, body)}`,
      "User-Agent": "GitHub-Hookshot/5c3e1b2",
    });
  };
  const frames = join(work, "frames");
  mkdirSync(frames);
  await cdp("Emulation.setDeviceMetricsOverride", { width: 1100, height: 620, deviceScaleFactor: 1, mobile: false });
  await cdp("Browser.setDownloadBehavior", { behavior: "allow", downloadPath: work });
  await cdp("Page.navigate", { url: `${UI}/e/${ev}` });
  await sleep(1200);
  const got = [];
  const onFrame = (m) => {
    const msg = JSON.parse(m.data);
    if (msg.method !== "Page.screencastFrame") return;
    got.push({ at: msg.params.metadata.timestamp, data: msg.params.data });
    cdp("Page.screencastFrameAck", { sessionId: msg.params.sessionId });
  };
  ws.addEventListener("message", onFrame);
  await cdp("Page.startScreencast", { format: "png", everyNthFrame: 1 });
  const click = (sel) => js(`document.querySelector(${JSON.stringify(sel)}).click()`);
  const reqID = async (no) => (await reqs(ev)).find((r) => r.no === no).id;
  await sleep(1500);
  await deliver({ action: "opened", number: 7, pull_request: { title: "Add retries to the exporter" } });
  await sleep(1400);
  await deliver({ action: "synchronize", number: 7, after: "4b8d0a7" });
  await sleep(1400);
  await deliver({ action: "labeled", number: 7, label: { name: "ready" } });   // the 3rd: 503
  await sleep(1600);
  await deliver({ action: "opened", number: 8, pull_request: { title: "Bump the SDK" } }, "an_old_secret");
  await sleep(1400);
  // Opened as after a reload: the register's verdict counts are not live yet.
  await cdp("Page.navigate", { url: `${UI}/e/${ev}/r/${await reqID(4)}` });   // the reason
  await sleep(3200);
  await deliver({ action: "closed", number: 7, pull_request: { merged: true } });
  await sleep(1000);
  await deliver({ action: "reopened", number: 8 });                            // the 6th: 503
  await sleep(1800);
  await cdp("Page.navigate", { url: `${UI}/e/${ev}?report` });
  await sleep(1800);
  await click("dialog[open] button[type=submit]");                             // Download report
  let report;
  for (let i = 0; i < 50 && !report; i++) {
    await sleep(100);
    report = readdirSync(work).find((f) => f.endsWith(".html") && f !== "report.html");
  }
  await sleep(600);
  await cdp("Page.navigate", { url: `file://${join(work, report)}` });
  await sleep(2600);
  await js("window.scrollTo({ top: 420, behavior: 'smooth' })");
  await sleep(2000);
  await cdp("Page.stopScreencast");
  ws.removeEventListener("message", onFrame);
  await cdp("Emulation.setDeviceMetricsOverride", { width: 1280, height: 800, deviceScaleFactor: 2, mobile: false });
  // Each frame lasts until the next one; the last holds a second.
  let list = "ffconcat version 1.0\n";
  got.forEach((f, i) => {
    const name = `f${String(i).padStart(5, "0")}.png`;
    writeFileSync(join(frames, name), Buffer.from(f.data, "base64"));
    const next = i + 1 < got.length ? got[i + 1].at : f.at + 1;
    list += `file ${name}\nduration ${Math.max(0.001, next - f.at).toFixed(3)}\n`;
  });
  list += `file f${String(got.length - 1).padStart(5, "0")}.png\n`;
  writeFileSync(join(frames, "list.txt"), list);
  execFileSync("docker", ["run", "--rm", "-v", `${frames}:/f`, "--entrypoint", "ffmpeg", FFMPEG, "-loglevel", "error", "-y",
    "-f", "concat", "-i", "/f/list.txt", "-vf",
    "fps=12,split[a][b];[a]palettegen=max_colors=96:stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle",
    "-loop", "0", "/f/demo.gif"], { stdio: "inherit" });
  const gif = readFileSync(join(frames, "demo.gif"));
  writeFileSync(join(outDir, "demo.gif"), gif);
  console.log(`${join(outDir, "demo.gif")}  ${Math.round(gif.length / 1024)} KiB, ${got.length} frames`);
}

// The repository's social preview (1280×640 PNG, uploaded by hand in the
// repository settings): the name, the line, and the strip shot above.
if (!only.length || only.includes("social")) {
  const media = join(outDir, "..", "media");
  mkdirSync(media, { recursive: true });
  const strip = resolve(outDir, "strip.webp");
  // The UI's fonts, inlined: a file:// page may not load them from the server.
  const vendor = new URL("../../internal/web/static/vendor/", import.meta.url);
  const font = (family, file, weight) => `@font-face{font-family:"${family}";font-weight:${weight};src:url(data:font/woff2;base64,${
    readFileSync(new URL(file, vendor)).toString("base64")}) format("woff2")}`;
  writeFileSync(join(work, "social.html"), `<!doctype html><meta charset="utf-8">
<style>
  ${font("Saira Stencil One", "saira-stencil-one/saira-stencil-one-latin-400-normal.woff2", 400)}
  ${font("Atkinson Hyperlegible", "atkinson-hyperlegible/atkinson-hyperlegible-latin-400-normal.woff2", 400)}
  ${font("Martian Mono", "martian-mono/martian-mono-latin-400-normal.woff2", 400)}
  html,body{margin:0;width:1280px;height:640px;overflow:hidden;background:#E6ECE7}
  main{box-sizing:border-box;padding:64px 72px;display:flex;flex-direction:column;gap:26px}
  h1{margin:0;font:400 116px/1 "Saira Stencil One",sans-serif;letter-spacing:.04em;color:#24308C;text-transform:uppercase}
  p{margin:0;font:400 42px/1.25 "Atkinson Hyperlegible",sans-serif;color:#17201B}
  small{font:400 21px/1.4 "Martian Mono",monospace;color:#58695F}
  /* the strip shot carries a margin of its neighbours: show the strip only */
  div{position:absolute;left:0;bottom:28px;width:1280px;height:150px;overflow:hidden}
  img{display:block;width:1280px;margin:-17px 0 0 0}
</style>
<main><h1>Hookwitness</h1>
<p>Catch webhooks, get a verdict, attach the evidence.</p>
<small>self-hosted · signature and JSON Schema verdicts · fault scenarios · reports</small></main>
<div><img src="file://${strip}"></div>`);
  await cdp("Emulation.setDeviceMetricsOverride", { width: 1280, height: 640, deviceScaleFactor: 1, mobile: false });
  await cdp("Page.navigate", { url: `file://${join(work, "social.html")}` });
  await sleep(1200);
  const { data } = await cdp("Page.captureScreenshot", { format: "png", clip: { x: 0, y: 0, width: 1280, height: 640, scale: 1 } });
  writeFileSync(join(media, "social-preview.png"), Buffer.from(data, "base64"));
  console.log(`${join(media, "social-preview.png")}  ${Math.round(Buffer.byteLength(data, "base64") / 1024)} KiB`);
  await cdp("Emulation.setDeviceMetricsOverride", { width: 1280, height: 800, deviceScaleFactor: 2, mobile: false });
}

// docs/troubleshooting.md, docs/deployment.md: the UI opened by a name that
// is not allowed.
await shot("host-refused", `http://hooks.example.com:${port}/admin/`, { maxHeight: 80 });

// docs/access.md: login, last, since the first user turns it on for the API
// too. First the endpoints page with login off; then the first admin, added
// through the API as the page shows, a user with a token, and the pages.
if (!only.length || ["login-off", "login", "users", "token-new"].some((n) => only.includes(n))) {
  if (!only.length || only.includes("login-off")) {
    if ((await api("GET", "/endpoints")).json.endpoints.length === 0) await api("POST", "/endpoints", { path: "/stripe/webhooks" });
  }
  await shot("login-off", `${UI}/`, { maxHeight: 420 });
  await api("POST", "/users", { name: "ann", role: "admin", password: "correct-horse-battery" });
  const login = await fetch(`${UI}/login`, { method: "POST", redirect: "manual",
    headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: "name=ann&password=correct-horse-battery" });
  const cookie = login.headers.getSetCookie().map((c) => c.split(";")[0]).join("; ");
  const as = { Cookie: cookie };
  await api("POST", "/users", { name: "bob", role: "user", password: "bob-password-123" }, as);
  await api("POST", "/users", { name: "carol", role: "admin", password: "carol-password-1" }, as);
  // Tokens of bob and ann, as their account pages would make them.
  const bobLogin = await fetch(`${UI}/login`, { method: "POST", redirect: "manual",
    headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: "name=bob&password=bob-password-123" });
  const bob = { Cookie: bobLogin.headers.getSetCookie().map((c) => c.split(";")[0]).join("; ") };
  await api("POST", "/me/tokens", { name: "ci-pipeline" }, bob);
  await api("POST", "/me/tokens", { name: "claude-code" }, as);
  await shot("login", `${UI}/login`, { maxHeight: 520,
    step: `document.querySelector('[name=name]').value = 'ann'` });
  // The browser logs in as ann through the form.
  await cdp("Page.navigate", { url: `${UI}/login` });
  await sleep(700);
  await js(`(() => { document.querySelector('[name=name]').value = 'ann';
    document.querySelector('[name=password]').value = 'correct-horse-battery';
    document.querySelector('form.card').submit(); })()`);
  await sleep(900);
  await shot("users", `${UI}/users`, { maxHeight: 900 });
  await shot("token-new", `${UI}/account?dialog=token`, { selector: dialog, step: steps(
    type("name", "agent"), "document.querySelector('dialog[open] form').submit()") });
}
ws.close();
process.exit(0);
