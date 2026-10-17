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

  // Each request appears once, whatever the feed resends; an "update"
  // replaces the row it names with its final outcome.
  document.addEventListener("htmx:sseBeforeMessage", (e) => {
    const ev = e.detail || {};
    if (ev.type === "update") {
      e.preventDefault();
      const tpl = document.createElement("template");
      tpl.innerHTML = String(ev.data || "").trim();
      const fresh = tpl.content.firstElementChild;
      const old = fresh && fresh.id && document.getElementById(fresh.id);
      if (!old) return;
      old.replaceWith(fresh);
      htmx.process(fresh);
      const ex = document.querySelector("#exhibit [data-exhibit]");
      if (ex && "req-" + ex.dataset.exhibit === fresh.id) {
        // The open exhibit shows the request that changed: reload it.
        htmx.ajax("GET", fresh.querySelector("a").getAttribute("href"), { target: "#exhibit", swap: "innerHTML" });
      }
      markSelected();
      return;
    }
    const id = ev.lastEventId;
    if (id && document.getElementById("req-" + id)) e.preventDefault();
  });
  document.addEventListener("htmx:sseMessage", (e) => {
    if (e.detail && e.detail.type === "update") return;
    const empty = document.getElementById("register-empty");
    if (empty) empty.remove();
    // The server prunes past the retention limit; so does the page.
    const reg = document.getElementById("register");
    const keep = Number(reg && reg.dataset.keep) || 0;
    const rows = reg ? reg.querySelectorAll(":scope > li.row") : [];
    if (keep > 0) for (let i = rows.length - 1; i >= keep; i--) rows[i].remove();
    const kept = document.getElementById("kept");
    if (kept) {
      const n = (Number(kept.textContent) || 0) + 1;
      kept.textContent = String(keep > 0 ? Math.min(n, keep) : n);
    }
    markSelected();
    draw();
  });

  // After rows change the server sends the register's totals ("counts"):
  // the kept count and the header's verdict counts follow them.
  document.addEventListener("htmx:sseOpen", (e) => {
    const src = e.detail && e.detail.source;
    if (!src || src.hwCounts) return;
    src.hwCounts = true;
    src.addEventListener("counts", (m) => {
      let c;
      try { c = JSON.parse(m.data); } catch { return; }
      document.querySelectorAll("[data-count]").forEach((el) => {
        const n = c[el.dataset.count];
        if (typeof n !== "number") return;
        el.textContent = String(n);
        if (el.dataset.count === "nc") el.parentElement.hidden = n === 0;
      });
      const form = document.querySelector(".verdict-counts form[data-confirm]");
      if (form && typeof c.kept === "number") {
        form.dataset.confirm = form.dataset.confirm.replace(/^Re-check the \d+ /, "Re-check the " + c.kept + " ");
      }
    });
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
    const by = e.submitter && e.submitter.dataset.confirm;
    const msg = by || (e.target.dataset && e.target.dataset.confirm);
    if (msg && !window.confirm(msg)) e.preventDefault();
  });

  // Editors are dialogs with their own URL: the page renders one open, here
  // it becomes modal; Escape goes back to the list it was opened from.
  document.querySelectorAll("dialog[data-modal]").forEach((d) => {
    d.close();
    d.showModal();
    d.addEventListener("cancel", (e) => {
      e.preventDefault();
      location.assign(d.dataset.close);
    });
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
        mark: li.dataset.mark || "ok",
        verdict: li.dataset.verdict || "",
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
    const pen = w - 28, base = h - 22;
    const x = (t) => pen - ((now - t) / WINDOW) * (pen - 8);
    const mono = css("--f-data");
    const ms = marks().filter((m) => m.at > now - WINDOW - 5000);
    ms.forEach((m) => (m.x = x(m.at)));
    ctx.font = "600 10px " + mono;
    const lay = window.HookwitnessStrip.layout(ms, w, h, pen, (s) => ctx.measureText(s).width);
    const top = lay.top;

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
    ctx.textAlign = "right"; // clear of the pen's arrow
    ctx.fillText("now", lay.now.x, h - 7);
    ctx.textAlign = "center";
    ctx.strokeStyle = css("--ink");
    ctx.globalAlpha = 0.35;
    ctx.beginPath();
    ctx.moveTo(0, base + 0.5);
    ctx.lineTo(pen, base + 0.5);
    ctx.stroke();
    ctx.globalAlpha = 1;

    // Brackets between deliveries of the same body, on rows (strip.js).
    lay.brackets.forEach((br) => {
      ctx.strokeStyle = css("--ink-soft");
      ctx.lineWidth = 1;
      ctx.setLineDash(br.crowded ? [1, 2] : [3, 3]);
      ctx.beginPath();
      if (br.openLeft) {
        ctx.moveTo(0, br.y);
      } else {
        ctx.moveTo(br.a, br.y + 6);
        ctx.lineTo(br.a, br.y);
      }
      ctx.lineTo(br.b, br.y);
      ctx.lineTo(br.b, br.y + 6);
      ctx.stroke();
      ctx.setLineDash([]);
    });
    // Labels after every bracket, edged with paper: a row's label sits
    // where the row above may draw its line.
    ctx.font = "600 10px " + mono;
    ctx.fillStyle = css("--ink");
    ctx.strokeStyle = css("--paper");
    ctx.lineWidth = 3;
    lay.brackets.forEach((br) => {
      if (!br.label) return;
      ctx.strokeText(br.label, br.lx, br.y - 4);
      ctx.fillText(br.label, br.lx, br.y - 4);
    });
    ms.forEach((m) => {
      const mx = x(m.at);
      if (mx < -4) return;
      const size = m.size > 0 ? m.size : 64;
      const hgt = Math.max(10, Math.min(base - top, (Math.log2(Math.max(size, 64)) - 6) * 9));
      const col = css(m.mark === "ok" ? "--ink" : "--alarm");
      const tip = base - hgt;
      if (m.verdict === "fail") {
        // Failed authorization: a red bar under the line, whatever the reply.
        ctx.fillStyle = css("--alarm");
        ctx.fillRect(mx - 4, base + 3, 8, 3);
      }
      ctx.strokeStyle = col;
      ctx.fillStyle = col;
      ctx.lineWidth = m.selected ? 3 : 2;
      if (m.mark === "timeout") {
        // A timeout: a dashed stem to the top, nothing came back.
        ctx.setLineDash([3, 3]);
        ctx.beginPath();
        ctx.moveTo(mx, base);
        ctx.lineTo(mx, top);
        ctx.stroke();
        ctx.setLineDash([]);
        ctx.beginPath();
        ctx.arc(mx, top, m.selected ? 5 : 4, 0, Math.PI * 2);
        ctx.stroke();
        return;
      }
      ctx.beginPath();
      ctx.moveTo(mx, base);
      ctx.lineTo(mx, tip);
      ctx.stroke();
      if (m.mark === "drop") {
        // A drop: the stem ends in a cross, the line was cut.
        const r = m.selected ? 6 : 5;
        ctx.beginPath();
        ctx.moveTo(mx - r, tip - r);
        ctx.lineTo(mx + r, tip + r);
        ctx.moveTo(mx + r, tip - r);
        ctx.lineTo(mx - r, tip + r);
        ctx.stroke();
        return;
      }
      ctx.beginPath();
      ctx.arc(mx, tip, m.selected ? 4 : 3, 0, Math.PI * 2);
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
  // Images under a request's body: a thumbnail opens the viewer, fitted to
  // the window; 1:1 shows the pixels, scrolling when larger. The exhibit is
  // swapped by htmx, so clicks are taken at the document.
  document.addEventListener("click", (e) => {
    const thumb = e.target.closest(".thumb[data-full]");
    const viewer = document.getElementById("viewer");
    if (thumb && viewer) {
      viewer.querySelector(".viewer-stage img").src = thumb.dataset.full;
      viewer.querySelector(".viewer-stage img").alt = "The image at " + thumb.dataset.title;
      viewer.querySelector(".viewer-title").textContent = thumb.dataset.title;
      const dl = viewer.querySelector('[data-viewer="download"]');
      dl.href = thumb.dataset.download || thumb.dataset.full;
      viewer.classList.remove("actual");
      viewer.querySelector('[data-viewer="zoom"]').setAttribute("aria-pressed", "false");
      viewer.showModal();
      return;
    }
    const ctl = e.target.closest("[data-viewer]");
    if (ctl && viewer && viewer.contains(ctl)) {
      if (ctl.dataset.viewer === "close") viewer.close();
      if (ctl.dataset.viewer === "zoom") {
        const on = viewer.classList.toggle("actual");
        ctl.setAttribute("aria-pressed", on ? "true" : "false");
      }
      return;
    }
    if (viewer && e.target === viewer) viewer.close(); // a click on the backdrop
  });

  markSelected();
})();
