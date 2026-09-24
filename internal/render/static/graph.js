// Null graph view — the one page with real JavaScript. A small
// force-directed layout on a canvas: no framework, no library, no build
// step. Nodes are notes coloured by tier; an edge is only as trustworthy
// as its weaker end, so an edge touching a dakhil note is drawn dashed.
(() => {
  "use strict";
  const TIERS = ["dakhil", "amil", "thabit", "asil"];
  const canvas = document.getElementById("graph");
  const ctx = canvas.getContext("2d");
  const legend = document.getElementById("legend");
  const css = getComputedStyle(document.documentElement);
  const color = (t) => css.getPropertyValue("--t-" + t).trim() || "#888";
  const fg = css.getPropertyValue("--fg").trim() || "#ccc";
  const dim = css.getPropertyValue("--fg-dim").trim() || "#888";

  let nodes = [], edges = [], byPath = new Map();
  const visible = new Set(TIERS);
  let view = { x: 0, y: 0, k: 1 }; // screen = world * k + (x, y)
  let hover = null, drag = null, pan = null, alpha = 1, raf = 0;
  let W = 0, H = 0;

  function resize() {
    const r = canvas.getBoundingClientRect(), d = window.devicePixelRatio || 1;
    W = r.width; H = r.height;
    canvas.width = Math.round(W * d); canvas.height = Math.round(H * d);
    ctx.setTransform(d, 0, 0, d, 0, 0);
    schedule();
  }

  function buildLegend(counts) {
    for (const t of TIERS) {
      const label = document.createElement("label");
      const box = document.createElement("input");
      box.type = "checkbox"; box.checked = true;
      box.addEventListener("change", () => {
        box.checked ? visible.add(t) : visible.delete(t);
        alpha = Math.max(alpha, 0.3); schedule();
      });
      const chip = document.createElement("span");
      chip.className = "chip t-" + t;
      chip.innerHTML = '<span class="dot"></span>' + t + " <b>" + (counts[t] || 0) + "</b>";
      label.append(box, chip);
      legend.append(label);
    }
  }

  function load(data) {
    nodes = data.nodes.map((n, i) => {
      const a = i * 2.399963, r = 14 * Math.sqrt(i + 1); // golden-angle spiral start
      return { path: n.path, title: n.title, tier: n.tier, x: Math.cos(a) * r, y: Math.sin(a) * r, vx: 0, vy: 0, deg: 0, fixed: false };
    });
    byPath = new Map(nodes.map((n) => [n.path, n]));
    edges = [];
    for (const e of data.edges) {
      const a = byPath.get(e.from), b = byPath.get(e.to);
      if (!a || !b) continue;
      a.deg++; b.deg++;
      edges.push({ a, b, tier: e.tier });
    }
    const counts = {};
    for (const n of nodes) counts[n.tier] = (counts[n.tier] || 0) + 1;
    buildLegend(counts);
    view = { x: W / 2, y: H / 2, k: 1 };
    alpha = 1; schedule();
  }

  // ---- simulation ---------------------------------------------------
  const MAXV = 30; // px per frame: a hard stop against a runaway layout
  const CUT = 110; // repulsion reaches this far; a grid keeps it near-linear
  function step() {
    const live = nodes.filter((n) => visible.has(n.tier));
    const grid = new Map();
    for (const n of live) {
      const key = Math.floor(n.x / CUT) + "," + Math.floor(n.y / CUT);
      (grid.get(key) || grid.set(key, []).get(key)).push(n);
    }
    for (const n of live) {
      const cx = Math.floor(n.x / CUT), cy = Math.floor(n.y / CUT);
      for (let gx = cx - 1; gx <= cx + 1; gx++) for (let gy = cy - 1; gy <= cy + 1; gy++) {
        const cell = grid.get(gx + "," + gy);
        if (!cell) continue;
        for (const m of cell) {
          if (m === n) continue;
          let dx = n.x - m.x, dy = n.y - m.y, d2 = dx * dx + dy * dy;
          if (d2 > CUT * CUT) continue;
          if (d2 < 0.01) { dx = Math.random() - 0.5; dy = Math.random() - 0.5; d2 = 0.25; }
          const f = (900 / (d2 + 30)) * alpha; // softened: two nearly-coincident nodes must not fire each other to infinity
          n.vx += dx * f; n.vy += dy * f;
        }
      }
    }
    for (const e of edges) {
      if (!visible.has(e.a.tier) || !visible.has(e.b.tier)) continue;
      const dx = e.b.x - e.a.x, dy = e.b.y - e.a.y, d = Math.sqrt(dx * dx + dy * dy) || 1;
      const f = (d - 60) * 0.04 * alpha;
      const fx = (dx / d) * f, fy = (dy / d) * f;
      e.a.vx += fx; e.a.vy += fy; e.b.vx -= fx; e.b.vy -= fy;
    }
    for (const n of live) {
      n.vx -= n.x * 0.012 * alpha; n.vy -= n.y * 0.012 * alpha; // gravity keeps components together
      if (n.fixed) { n.vx = n.vy = 0; continue; }
      n.vx *= 0.6; n.vy *= 0.6;
      const sp = Math.hypot(n.vx, n.vy);
      if (sp > MAXV) { n.vx *= MAXV / sp; n.vy *= MAXV / sp; }
      n.x += n.vx; n.y += n.vy;
      if (!Number.isFinite(n.x) || !Number.isFinite(n.y)) { n.x = (Math.random() - 0.5) * 50; n.y = (Math.random() - 0.5) * 50; n.vx = n.vy = 0; }
    }
    alpha *= 0.985;
  }

  // ---- drawing ------------------------------------------------------
  const radius = (n) => 3.5 + Math.min(7, Math.sqrt(n.deg) * 1.6);
  function draw() {
    ctx.clearRect(0, 0, W, H);
    ctx.save();
    ctx.translate(view.x, view.y); ctx.scale(view.k, view.k);
    const near = new Set();
    if (hover) { near.add(hover); for (const e of edges) { if (e.a === hover) near.add(e.b); if (e.b === hover) near.add(e.a); } }
    ctx.lineWidth = 1 / view.k;
    for (const e of edges) {
      if (!visible.has(e.a.tier) || !visible.has(e.b.tier)) continue;
      const lit = hover && (e.a === hover || e.b === hover);
      ctx.strokeStyle = lit ? fg : color(e.tier);
      ctx.globalAlpha = hover ? (lit ? 0.95 : 0.12) : 0.55;
      ctx.setLineDash(e.tier === "dakhil" ? [4 / view.k, 3 / view.k] : []);
      ctx.beginPath(); ctx.moveTo(e.a.x, e.a.y); ctx.lineTo(e.b.x, e.b.y); ctx.stroke();
    }
    ctx.setLineDash([]);
    for (const n of nodes) {
      if (!visible.has(n.tier)) continue;
      ctx.globalAlpha = hover && !near.has(n) ? 0.25 : 1;
      ctx.beginPath(); ctx.arc(n.x, n.y, radius(n), 0, 6.2832);
      if (n.tier === "dakhil") { ctx.strokeStyle = color(n.tier); ctx.lineWidth = 1.6 / view.k; ctx.stroke(); }
      else { ctx.fillStyle = color(n.tier); ctx.fill(); }
      if (n === hover) { ctx.strokeStyle = fg; ctx.lineWidth = 2 / view.k; ctx.stroke(); }
    }
    ctx.globalAlpha = 1;
    const fontPx = 12 / view.k;
    ctx.font = fontPx + "px system-ui, sans-serif"; ctx.textBaseline = "middle";
    for (const n of nodes) {
      if (!visible.has(n.tier)) continue;
      if (!(near.has(n) || (view.k > 1.6 && nodes.length < 400))) continue;
      const tw = ctx.measureText(n.title).width;
      ctx.fillStyle = "rgba(20,22,26,0.75)"; ctx.fillRect(n.x + radius(n) + 2 / view.k, n.y - fontPx * 0.7, tw + 6 / view.k, fontPx * 1.4);
      ctx.fillStyle = n === hover ? fg : dim; ctx.fillText(n.title, n.x + radius(n) + 5 / view.k, n.y);
    }
    ctx.restore();
  }

  function frame() {
    raf = 0;
    if (alpha > 0.005 || drag) { step(); }
    draw();
    if (alpha > 0.005 || drag) schedule();
  }
  function schedule() { if (!raf) raf = requestAnimationFrame(frame); }

  // ---- interaction --------------------------------------------------
  const toWorld = (sx, sy) => ({ x: (sx - view.x) / view.k, y: (sy - view.y) / view.k });
  function pick(sx, sy) {
    const p = toWorld(sx, sy);
    let best = null, bd = Infinity;
    for (const n of nodes) {
      if (!visible.has(n.tier)) continue;
      const d = Math.hypot(n.x - p.x, n.y - p.y), r = radius(n) + 3 / view.k;
      if (d <= r && d < bd) { best = n; bd = d; }
    }
    return best;
  }
  const local = (ev) => { const r = canvas.getBoundingClientRect(); return [ev.clientX - r.left, ev.clientY - r.top]; };

  canvas.addEventListener("pointerdown", (ev) => {
    const [sx, sy] = local(ev), n = pick(sx, sy);
    canvas.setPointerCapture(ev.pointerId);
    if (n) { drag = { n, moved: false, sx, sy }; n.fixed = true; }
    else { pan = { sx, sy, vx: view.x, vy: view.y }; canvas.classList.add("dragging"); }
  });
  canvas.addEventListener("pointermove", (ev) => {
    const [sx, sy] = local(ev);
    if (drag) {
      if (Math.hypot(sx - drag.sx, sy - drag.sy) > 4) drag.moved = true;
      const p = toWorld(sx, sy); drag.n.x = p.x; drag.n.y = p.y; alpha = Math.max(alpha, 0.2); schedule();
    } else if (pan) {
      view.x = pan.vx + (sx - pan.sx); view.y = pan.vy + (sy - pan.sy); schedule();
    } else {
      const n = pick(sx, sy);
      if (n !== hover) { hover = n; canvas.style.cursor = n ? "pointer" : ""; schedule(); }
    }
  });
  canvas.addEventListener("pointerup", () => {
    if (drag) {
      const { n, moved } = drag; n.fixed = false; drag = null;
      if (!moved) window.location.href = "/n/" + n.path.split("/").map(encodeURIComponent).join("/");
    }
    pan = null; canvas.classList.remove("dragging");
  });
  canvas.addEventListener("wheel", (ev) => {
    ev.preventDefault();
    const [sx, sy] = local(ev), k = Math.min(6, Math.max(0.15, view.k * Math.exp(-ev.deltaY * 0.0015)));
    view.x = sx - ((sx - view.x) / view.k) * k; view.y = sy - ((sy - view.y) / view.k) * k; view.k = k;
    schedule();
  }, { passive: false });
  window.addEventListener("resize", resize);

  resize();
  fetch("/graph/data", { credentials: "same-origin" })
    .then((r) => { if (!r.ok) throw new Error(r.status); return r.json(); })
    .then(load)
    .catch((e) => { legend.textContent = "could not load the graph (" + e.message + ")"; });
})();
