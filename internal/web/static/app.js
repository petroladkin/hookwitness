// Hookwitness UI behaviour: the live feed's bookkeeping, the recorder strip,
// and the small things plain HTML cannot do (confirm, navigate on select).
(function () {
  "use strict";

  const reduce = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  // The newest request already on the page; a feed (re)opened from scratch
  // starts after it. A browser's own reconnect sends Last-Event-ID instead.
  function newestShown() {
    let max = 0;
    document.querySelectorAll("#register > li[data-id]").forEach((li) => {
      max = Math.max(max, Number(li.dataset.id) || 0);
    });
    return max;
  }
  if (window.htmx) {
    htmx.createEventSource = function (url) {
      const u = new URL(url, location.href);
      u.searchParams.set("after", String(newestShown()));
      return new EventSource(u.toString());
    };
  }

  const state = document.getElementById("feed-state");
  function feed(s, label) {
    if (!state) return;
    state.dataset.state = s;
    state.lastElementChild.textContent = label;
  }

  document.addEventListener("htmx:sseOpen", () => feed("live", "Recording"));
  document.addEventListener("htmx:sseError", () => feed("lost", "Reconnecting"));

  // Each request appears once, whatever the feed resends.
  document.addEventListener("htmx:sseBeforeMessage", (e) => {
    const id = e.detail && e.detail.lastEventId;
    if (id && document.getElementById("req-" + id)) e.preventDefault();
  });
  document.addEventListener("htmx:sseMessage", () => {
    const empty = document.getElementById("register-empty");
    if (empty) empty.remove();
    const kept = document.getElementById("kept");
    if (kept) kept.textContent = String((Number(kept.textContent) || 0) + 1);
    markSelected();
    draw();
  });

  // The row of the exhibit on show is marked in the register.
  function markSelected() {
    const ex = document.querySelector("#exhibit [data-exhibit]");
    const id = ex ? ex.dataset.exhibit : "";
    document.querySelectorAll("#register > li.row").forEach((li) => {
      if (li.dataset.id === id) li.setAttribute("aria-current", "true");
      else li.removeAttribute("aria-current");
    });
    draw();
  }
  const narrow = window.matchMedia("(max-width: 820px)");
  document.addEventListener("htmx:afterSwap", (e) => {
    if (!e.target) return;
    if (e.target.id === "exhibit") {
      markSelected();
      // In one column the exhibit is below the register: bring it up.
      if (narrow.matches) e.target.scrollIntoView({ block: "start", behavior: reduce ? "auto" : "smooth" });
    } else if (e.target.closest("#register")) {
      markSelected();
    }
  });

  document.addEventListener("change", (e) => {
    const el = e.target;
    if (el.matches && el.matches("select[data-navigate]") && el.value) location.assign(el.value);
  });
  document.addEventListener("submit", (e) => {
    const msg = e.target.dataset && e.target.dataset.confirm;
    if (msg && !window.confirm(msg)) e.preventDefault();
  });

  // The strip: the last two minutes on recorder paper, the pen on the right,
  // a mark per request (height = body size, log scale; refused in red), and
  // deliveries of the same body bracketed with the gap between them.
  const cv = document.getElementById("trace");
  const WINDOW = 120000;
  let ctx = cv ? cv.getContext("2d") : null;

  function css(v) {
    return getComputedStyle(document.documentElement).getPropertyValue(v).trim();
  }
  function marks() {
    const out = [];
    document.querySelectorAll("#register > li.row").forEach((li) => {
      out.push({
        at: Number(li.dataset.at),
        size: Number(li.dataset.size),
        bad: li.dataset.bad === "1",
        group: li.dataset.group || "",
        selected: li.getAttribute("aria-current") === "true",
      });
    });
    return out.sort((a, b) => a.at - b.at);
  }

  function draw() {
    if (!ctx) return;
    const now = Date.now();
    const dpr = window.devicePixelRatio || 1;
    const w = cv.clientWidth, h = cv.clientHeight;
    if (cv.width !== Math.round(w * dpr) || cv.height !== Math.round(h * dpr)) {
      cv.width = Math.round(w * dpr);
      cv.height = Math.round(h * dpr);
    }
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    const pen = w - 28, base = h - 22, top = 26;
    const x = (t) => pen - ((now - t) / WINDOW) * (pen - 8);
    const mono = css("--f-data");

    ctx.fillStyle = css("--paper");
    ctx.fillRect(0, 0, w, h);
    // Grid: a line every 2 s, a major one every 10 s, moving with the paper.
    for (let s = 0; s <= WINDOW / 1000; s += 2) {
      const t = now - (now % 2000) - s * 1000;
      const major = Math.round(t / 1000) % 10 === 0;
      ctx.strokeStyle = css(major ? "--grid-major" : "--grid");
      ctx.lineWidth = 1;
      ctx.beginPath();
      ctx.moveTo(Math.round(x(t)) + 0.5, top - 10);
      ctx.lineTo(Math.round(x(t)) + 0.5, base);
      ctx.stroke();
    }
    ctx.strokeStyle = css("--grid");
    for (let y = base; y >= top - 10; y -= 16) {
      ctx.beginPath();
      ctx.moveTo(0, Math.round(y) + 0.5);
      ctx.lineTo(pen, Math.round(y) + 0.5);
      ctx.stroke();
    }
    ctx.fillStyle = css("--muted");
    ctx.font = "10px " + mono;
    ctx.textAlign = "center";
    for (let s = 30; s < WINDOW / 1000; s += 30) ctx.fillText("−" + s + " s", x(now - s * 1000), h - 7);
    ctx.fillText("now", pen, h - 7);
    ctx.strokeStyle = css("--ink");
    ctx.globalAlpha = 0.35;
    ctx.beginPath();
    ctx.moveTo(0, base + 0.5);
    ctx.lineTo(pen, base + 0.5);
    ctx.stroke();
    ctx.globalAlpha = 1;

    const ms = marks().filter((m) => m.at > now - WINDOW - 5000);
    // Brackets between deliveries of the same body.
    const last = {};
    ms.forEach((m) => {
      if (!m.group) return;
      const prev = last[m.group];
      last[m.group] = m;
      if (!prev) return;
      const a = x(prev.at), b = x(m.at), y = top - 4;
      if (b < 0) return;
      ctx.strokeStyle = css("--ink-soft");
      ctx.lineWidth = 1;
      ctx.setLineDash([3, 3]);
      ctx.beginPath();
      ctx.moveTo(a, y + 6);
      ctx.lineTo(a, y);
      ctx.lineTo(b, y);
      ctx.lineTo(b, y + 6);
      ctx.stroke();
      ctx.setLineDash([]);
      ctx.fillStyle = css("--ink");
      ctx.font = "600 10px " + mono;
      ctx.fillText("+" + ((m.at - prev.at) / 1000).toFixed(1) + " s", (a + b) / 2, y - 4);
    });
    ms.forEach((m) => {
      const mx = x(m.at);
      if (mx < -4) return;
      const size = m.size > 0 ? m.size : 64;
      const hgt = Math.max(10, Math.min(base - top, (Math.log2(Math.max(size, 64)) - 6) * 9));
      const col = css(m.bad ? "--alarm" : "--ink");
      ctx.strokeStyle = col;
      ctx.lineWidth = m.selected ? 3 : 2;
      ctx.beginPath();
      ctx.moveTo(mx, base);
      ctx.lineTo(mx, base - hgt);
      ctx.stroke();
      ctx.fillStyle = col;
      ctx.beginPath();
      ctx.arc(mx, base - hgt, m.selected ? 4 : 3, 0, Math.PI * 2);
      ctx.fill();
    });
    // The pen.
    ctx.strokeStyle = css("--alarm");
    ctx.lineWidth = 2;
    ctx.beginPath();
    ctx.moveTo(pen + 0.5, top - 12);
    ctx.lineTo(pen + 0.5, base + 4);
    ctx.stroke();
    ctx.fillStyle = css("--alarm");
    ctx.beginPath();
    ctx.moveTo(pen - 5, base + 4);
    ctx.lineTo(pen + 6, base + 4);
    ctx.lineTo(pen + 0.5, base + 12);
    ctx.closePath();
    ctx.fill();
  }

  if (cv) {
    if (reduce) {
      // No paper feed: redraw on arrivals and every few seconds.
      setInterval(draw, 5000);
    } else {
      const loop = () => {
        draw();
        requestAnimationFrame(loop);
      };
      requestAnimationFrame(loop);
    }
    window.addEventListener("resize", draw);
    window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", draw);
  }
  markSelected();
})();
