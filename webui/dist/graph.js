// graph.js - self-contained Canvas2D force-directed graph with a Barnes-Hut
// quadtree for repulsion. No dependencies. ~1-5K nodes at 60fps.

const REDUCED = matchMedia('(prefers-reduced-motion: reduce)').matches;

function cssVar(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

export function colorForKind(kind) {
  const k = (kind || '').toLowerCase();
  if (/func|method|constructor|closure/.test(k)) return cssVar('--k-function');
  if (/class|struct|interface|type|enum|trait|record|protocol/.test(k)) return cssVar('--k-type');
  if (/const|var|field|property|constant|literal|enum_member/.test(k)) return cssVar('--k-value');
  if (/module|package|file|namespace|entrypoint|entry|main/.test(k)) return cssVar('--k-entry');
  return cssVar('--k-other');
}

// nodeColor prefers an explicit node.color (feature graphs set one per group);
// otherwise it falls back to the code-kind palette (the semantic graph).
export function nodeColor(n) { return (n && n.color) || colorForKind(n && n.kind); }

export function createGraph(canvas, handlers = {}) {
  const ctx = canvas.getContext('2d');
  let nodes = [], edges = [], byId = new Map();
  let cam = { scale: 1, tx: 0, ty: 0 };
  let alpha = 0, alphaTarget = 0;
  // alphaDecay controls how many ticks the sim runs before freezing. Big graphs
  // rebuild a quadtree every tick, so we settle them in far fewer ticks (set in
  // setData) to keep large views from janking for tens of seconds.
  let alphaDecay = 1 - Math.pow(0.001, 1 / 300);
  const velocityDecay = 0.6;
  let hover = null, selected = null, focusSet = null;
  let replayActive = null, replayVisited = null; // Sets of node ids during session replay
  let replayLead = null; // top-degree active ids that get labels/rings (a file can touch 100s of symbols)
  let autoFit = false; // keep the view framed while the sim settles (any graph size)
  let dirty = true, W = 0, H = 0, dpr = 1;

  // ---------- sizing ----------
  function resize() {
    dpr = Math.min(window.devicePixelRatio || 1, 2);
    const r = canvas.getBoundingClientRect();
    W = r.width; H = r.height;
    canvas.width = Math.round(W * dpr);
    canvas.height = Math.round(H * dpr);
    dirty = true;
  }
  new ResizeObserver(resize).observe(canvas);
  resize();

  // ---------- data ----------
  // Steeper degree scale so hubs clearly outsize leaves (the eye lands on them
  // first); floor keeps isolated nodes visible + clickable, cap bounds mega-hubs.
  function radius(n) { return Math.min(3.5 + Math.sqrt(n.degree || 0) * 2, 22); }

  function setData(rawNodes, rawEdges) {
    byId = new Map();
    nodes = rawNodes.map((d) => {
      const angle = Math.random() * Math.PI * 2, rad = 60 + Math.random() * 380;
      const n = { ...d, x: Math.cos(angle) * rad, y: Math.sin(angle) * rad, vx: 0, vy: 0, degree: 0, fixed: false };
      byId.set(n.id, n);
      return n;
    });
    edges = [];
    for (const e of rawEdges) {
      const s = byId.get(e.from), t = byId.get(e.to);
      if (!s || !t || s === t) continue;
      s.degree++; t.degree++;
      edges.push({ ...e, source: s, target: t });
    }
    selected = null; hover = null; focusSet = null;
    // Fewer settling ticks for very large graphs (each tick rebuilds the quadtree);
    // the coverage-capped edge set keeps the layout well-spread regardless.
    const iters = nodes.length > 6000 ? 220 : 300;
    alphaDecay = 1 - Math.pow(0.001, 1 / iters);
    reheat(1);
    autoFit = true; // track the graph as it expands, until it settles or the user grabs it
    zoomToFit(false);
  }

  function mergeData(rawNodes, rawEdges) {
    let added = 0;
    const anchor = selected || nodes[0] || { x: 0, y: 0 };
    for (const d of rawNodes) {
      if (byId.has(d.id)) continue;
      const a = Math.random() * Math.PI * 2, rad = 30 + Math.random() * 40;
      const n = { ...d, x: anchor.x + Math.cos(a) * rad, y: anchor.y + Math.sin(a) * rad, vx: 0, vy: 0, degree: 0, fixed: false };
      byId.set(n.id, n); nodes.push(n); added++;
    }
    const seen = new Set(edges.map((e) => e.from + ' ' + e.to + ' ' + e.type));
    for (const e of rawEdges) {
      const key = e.from + ' ' + e.to + ' ' + e.type;
      if (seen.has(key)) continue;
      const s = byId.get(e.from), t = byId.get(e.to);
      if (!s || !t || s === t) continue;
      s.degree++; t.degree++; edges.push({ ...e, source: s, target: t }); seen.add(key);
    }
    reheat(0.5);
    return added;
  }

  // ---------- simulation ----------
  function reheat(a = 0.6) { alpha = Math.max(alpha, a); dirty = true; if (REDUCED) settle(); }
  function settle() { for (let i = 0; i < 400 && alpha > 0.001; i++) tick(); alpha = 0; dirty = true; }

  function buildQuadtree() {
    let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
    for (const n of nodes) { if (n.x < x0) x0 = n.x; if (n.y < y0) y0 = n.y; if (n.x > x1) x1 = n.x; if (n.y > y1) y1 = n.y; }
    if (!isFinite(x0)) return null;
    const size = Math.max(x1 - x0, y1 - y0, 1) + 1;
    const root = { cx: (x0 + x1) / 2, cy: (y0 + y1) / 2, half: size / 2, mass: 0, mx: 0, my: 0, node: null, kids: null };
    const insert = (q, n, depth) => {
      q.mass++; q.mx += n.x; q.my += n.y;
      if (!q.kids && !q.node) { q.node = n; return; }
      if (!q.kids) {
        q.kids = [null, null, null, null];
        const old = q.node; q.node = null;
        if (old) place(q, old, depth);
      }
      place(q, n, depth);
    };
    const place = (q, n, depth) => {
      const i = (n.x >= q.cx ? 1 : 0) + (n.y >= q.cy ? 2 : 0);
      let k = q.kids[i];
      if (!k) {
        const h = q.half / 2;
        k = q.kids[i] = { cx: q.cx + (i & 1 ? h : -h), cy: q.cy + (i & 2 ? h : -h), half: h, mass: 0, mx: 0, my: 0, node: null, kids: null };
      }
      if (depth > 24) { k.mass++; k.mx += n.x; k.my += n.y; k.node = k.node || n; return; }
      insert(k, n, depth + 1);
    };
    for (const n of nodes) insert(root, n, 0);
    return root;
  }

  function applyCharge() {
    const root = buildQuadtree();
    if (!root) return;
    // Repulsion must grow with the node count, or dense graphs (many edges pulling
    // inward) collapse along one axis into a spindle instead of spreading in 2D.
    const chargeScale = Math.min(1 + nodes.length / 500, 4.5);
    const theta2 = 0.81;
    const strength = -34 * chargeScale * alpha;
    for (const n of nodes) {
      const stack = [root];
      while (stack.length) {
        const q = stack.pop();
        if (!q || q.mass === 0) continue;
        const cxm = q.mx / q.mass, cym = q.my / q.mass;
        let dx = cxm - n.x, dy = cym - n.y;
        let d2 = dx * dx + dy * dy;
        const w = q.half * 2;
        if (q.node === n && !q.kids) continue;
        if (!q.kids || (w * w) / (d2 || 1e-6) < theta2) {
          if (d2 < 1) { d2 = 1; dx += (Math.random() - 0.5) * 0.5; dy += (Math.random() - 0.5) * 0.5; }
          const f = (strength * q.mass) / d2;
          n.vx += dx * f; n.vy += dy * f;
        } else {
          for (const k of q.kids) if (k) stack.push(k);
        }
      }
    }
  }

  function applyLinks() {
    // Weaken each link by the lesser endpoint degree (d3's bias): without this,
    // a hub with hundreds of edges gets pulled inward hundreds of times and
    // collapses dense graphs into a cigar. Leaves (degree 1) stay firmly attached.
    const dist = 46, stiff = 0.32 * alpha;
    for (const e of edges) {
      const s = e.source, t = e.target;
      let dx = t.x - s.x, dy = t.y - s.y;
      let d = Math.sqrt(dx * dx + dy * dy) || 1e-6;
      // Relax leaves (÷degree) but FLOOR the denominator so hub<->hub links never
      // go to ~zero — otherwise mega-hubs decouple, fly apart on repulsion, and
      // string the graph into a diagonal tail. Floor of 10 keeps hubs bound.
      const k = stiff / Math.min(Math.min(s.degree || 1, t.degree || 1), 10);
      const f = ((d - dist) / d) * k;
      dx *= f; dy *= f;
      if (!t.fixed) { t.vx -= dx; t.vy -= dy; }
      if (!s.fixed) { s.vx += dx; s.vy += dy; }
    }
  }

  function applyGravity() {
    // Gentle centering keeps the graph on-screen; too strong and it fights the
    // repulsion and helps the collapse, so keep it light for large graphs.
    const g = (nodes.length > 700 ? 0.022 : 0.045) * alpha;
    for (const n of nodes) { n.vx -= n.x * g; n.vy -= n.y * g; }
  }

  function tick() {
    if (alpha < 0.001) return;
    alpha += (alphaTarget - alpha) * alphaDecay;
    applyCharge(); applyLinks(); applyGravity();
    const maxV = 60; // clamp per-tick velocity so strong forces can't fling nodes to infinity
    for (const n of nodes) {
      if (n.fixed) { n.vx = 0; n.vy = 0; continue; }
      n.vx *= velocityDecay; n.vy *= velocityDecay;
      if (n.vx > maxV) n.vx = maxV; else if (n.vx < -maxV) n.vx = -maxV;
      if (n.vy > maxV) n.vy = maxV; else if (n.vy < -maxV) n.vy = -maxV;
      n.x += n.vx; n.y += n.vy;
    }
    dirty = true;
  }

  // ---------- camera ----------
  const toScreen = (x, y) => [(x * cam.scale) + cam.tx, (y * cam.scale) + cam.ty];
  const toWorld = (sx, sy) => [(sx - cam.tx) / cam.scale, (sy - cam.ty) / cam.scale];

  function zoomToFit(animate = true) {
    if (!nodes.length) return;
    let x0, y0, x1, y1;
    if (nodes.length >= 12) {
      // Robust bounds: frame the 2nd–98th percentile so a handful of weakly-linked
      // drifters can't blow up the bounding box and squash the core into a sliver.
      const xs = nodes.map((n) => n.x).sort((a, b) => a - b);
      const ys = nodes.map((n) => n.y).sort((a, b) => a - b);
      const lo = Math.floor(nodes.length * 0.02), hi = Math.min(nodes.length - 1, Math.ceil(nodes.length * 0.98));
      x0 = xs[lo]; x1 = xs[hi]; y0 = ys[lo]; y1 = ys[hi];
    } else {
      x0 = Infinity; y0 = Infinity; x1 = -Infinity; y1 = -Infinity;
      for (const n of nodes) { x0 = Math.min(x0, n.x); y0 = Math.min(y0, n.y); x1 = Math.max(x1, n.x); y1 = Math.max(y1, n.y); }
    }
    const pad = 80, gw = Math.max(x1 - x0, 1), gh = Math.max(y1 - y0, 1);
    const s = Math.min((W - pad) / gw, (H - pad) / gh, 2.2);
    const cx = (x0 + x1) / 2, cy = (y0 + y1) / 2;
    const target = { scale: s, tx: W / 2 - cx * s, ty: H / 2 - cy * s };
    animate && !REDUCED ? tweenCam(target) : (cam = target, dirty = true);
  }

  let camTween = null;
  function tweenCam(target) {
    const from = { ...cam }, t0 = performance.now(), dur = 420;
    camTween = () => {
      const p = Math.min((performance.now() - t0) / dur, 1);
      const e = 1 - Math.pow(1 - p, 3);
      cam.scale = from.scale + (target.scale - from.scale) * e;
      cam.tx = from.tx + (target.tx - from.tx) * e;
      cam.ty = from.ty + (target.ty - from.ty) * e;
      dirty = true;
      if (p >= 1) camTween = null;
    };
  }

  function focus(id) {
    const n = byId.get(id); if (!n) return;
    autoFit = false;
    selected = n;
    focusSet = new Set([n.id]);
    for (const e of edges) { if (e.source === n) focusSet.add(e.target.id); if (e.target === n) focusSet.add(e.source.id); }
    const target = { scale: Math.max(cam.scale, 1.1), tx: W / 2 - n.x * Math.max(cam.scale, 1.1), ty: H / 2 - n.y * Math.max(cam.scale, 1.1) };
    REDUCED ? (cam = target) : tweenCam(target);
    dirty = true;
  }
  function clearFocus() { selected = null; focusSet = null; dirty = true; }

  // ---------- replay ----------
  function setReplay(activeIds, visitedIds) {
    autoFit = false;
    replayActive = new Set(activeIds || []);
    replayVisited = new Set(visitedIds || []);
    // A single step is one file, which can map to hundreds of symbols. Only the
    // few highest-degree ones get a label + ring + glow; the rest just brighten,
    // so the step reads as "this cluster lit up" instead of a wall of text.
    const active = (activeIds || []).map((id) => byId.get(id)).filter(Boolean);
    active.sort((a, b) => (b.degree || 0) - (a.degree || 0));
    replayLead = new Set(active.slice(0, 5).map((n) => n.id));
    dirty = true;
  }
  function clearReplay() { replayActive = null; replayVisited = null; replayLead = null; dirty = true; }
  function panToIds(ids, scale) {
    autoFit = false;
    const pts = (ids || []).map((id) => byId.get(id)).filter(Boolean);
    if (!pts.length) return;
    let cx = 0, cy = 0;
    for (const n of pts) { cx += n.x; cy += n.y; }
    cx /= pts.length; cy /= pts.length;
    const sc = scale || Math.max(cam.scale, 1.0);
    const target = { scale: sc, tx: W / 2 - cx * sc, ty: H / 2 - cy * sc };
    REDUCED ? (cam = target, dirty = true) : tweenCam(target);
  }

  // ---------- rendering ----------
  function draw() {
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, W, H);
    const showLabels = cam.scale > 1.15;
    const replaying = replayActive !== null;
    const pulse = replaying ? (0.5 + 0.5 * Math.sin(performance.now() / 240)) : 0;
    const hi = hover || selected;
    const incident = hi ? new Set() : null;
    if (hi) for (const e of edges) { if (e.source === hi || e.target === hi) { incident.add(e); } }

    // edges
    ctx.lineWidth = 1;
    for (const e of edges) {
      const [x1, y1] = toScreen(e.source.x, e.source.y);
      const [x2, y2] = toScreen(e.target.x, e.target.y);
      let a = 0.16;
      let col = '185,185,185';
      if (replaying) {
        const both = replayVisited.has(e.source.id) && replayVisited.has(e.target.id);
        a = both ? 0.55 : 0.03;
        if (both) col = hexToRgb(nodeColor(e.source));
      } else if (hi) {
        if (incident.has(e)) { a = 0.85; col = hexToRgb(nodeColor(e.source)); }
        else a = 0.04;
      } else if (focusSet && (focusSet.has(e.source.id) && focusSet.has(e.target.id))) {
        a = 0.5;
      }
      ctx.strokeStyle = `rgba(${col},${a})`;
      ctx.beginPath(); ctx.moveTo(x1, y1); ctx.lineTo(x2, y2); ctx.stroke();
      if (incident && incident.has(e)) drawArrow(x1, y1, x2, y2, radius(e.target) * cam.scale);
    }

    // nodes
    for (const n of nodes) {
      const [x, y] = toScreen(n.x, n.y);
      if (x < -50 || y < -50 || x > W + 50 || y > H + 50) continue;
      const r = Math.max(radius(n) * cam.scale, 1.5);
      const col = nodeColor(n);
      let alpha = 1, isActive = false, isLead = false;
      if (replaying) {
        isActive = replayActive.has(n.id);
        isLead = isActive && replayLead && replayLead.has(n.id);
        alpha = isActive ? 1 : (replayVisited.has(n.id) ? 0.42 : 0.1);
      } else {
        const dim = hi && n !== hi && !(incident && [...incident].some((e) => e.source === n || e.target === n));
        const focusDim = !hi && focusSet && !focusSet.has(n.id);
        // At overview zoom (labels hidden, nothing hovered/focused) gently fade the
        // low-degree leaves so hubs/clusters pop and the eye lands on what matters.
        // Dim, don't delete; and never fight the hover/focus/replay alpha above.
        const overviewDim = !hi && !focusSet && !showLabels && (n.degree || 0) <= 1;
        // Keep dimmed context clearly visible (0.5, not near-invisible) so moving
        // the cursor over a sparse graph doesn't make everything flicker away.
        alpha = (dim || focusDim) ? 0.5 : (overviewDim ? 0.55 : 1);
      }
      if (isLead) { // expanding pulse ring on the few lead symbols only (not all 100s)
        ctx.globalAlpha = 0.55 * (1 - pulse * 0.7);
        ctx.beginPath(); ctx.arc(x, y, r + 4 + pulse * 11, 0, Math.PI * 2);
        ctx.strokeStyle = col; ctx.lineWidth = 2; ctx.stroke();
      }
      ctx.globalAlpha = alpha;
      if (n === hi || n === selected || isLead) { ctx.shadowColor = col; ctx.shadowBlur = isLead ? 18 : 16; }
      ctx.beginPath(); ctx.arc(x, y, isActive ? r + 1.5 : r, 0, Math.PI * 2);
      ctx.fillStyle = col; ctx.fill();
      ctx.shadowBlur = 0;
      if (n === selected) { ctx.lineWidth = 2; ctx.strokeStyle = '#fff'; ctx.stroke(); }
      else if (n.fixed) { ctx.lineWidth = 1.5; ctx.strokeStyle = 'rgba(255,255,255,0.5)'; ctx.stroke(); }
      // During replay label ONLY the few lead symbols of the active step — a file
      // can touch hundreds of symbols and labeling all of them is an unreadable wall.
      // Label hubs at zoom (degree >= 5); the biggest hubs (degree >= 8) get a
      // label a little earlier — even at overview — to orient the eye without a
      // wall of text.
      const wantLabel = n === hi || n === selected || isLead || (!replaying && ((showLabels && n.degree >= 5) || n.degree >= 8));
      if (wantLabel) {
        ctx.globalAlpha = alpha < 0.5 ? 0.25 : 0.92;
        ctx.fillStyle = '#f3f3f3';
        ctx.font = '11px ui-monospace, Menlo, monospace';
        ctx.textAlign = 'center';
        const label = n.name || n.id;
        ctx.fillText(label.length > 28 ? label.slice(0, 27) + '...' : label, x, y - (isActive ? r + 2 : r) - 5);
      }
      ctx.globalAlpha = 1;
    }
  }

  function drawArrow(x1, y1, x2, y2, back) {
    const ang = Math.atan2(y2 - y1, x2 - x1);
    const ex = x2 - Math.cos(ang) * (back + 3), ey = y2 - Math.sin(ang) * (back + 3);
    const s = 6;
    ctx.fillStyle = 'rgba(242,83,51,0.9)';
    ctx.beginPath();
    ctx.moveTo(ex, ey);
    ctx.lineTo(ex - Math.cos(ang - 0.4) * s, ey - Math.sin(ang - 0.4) * s);
    ctx.lineTo(ex - Math.cos(ang + 0.4) * s, ey - Math.sin(ang + 0.4) * s);
    ctx.closePath(); ctx.fill();
  }

  function hexToRgb(hex) {
    hex = (hex || '').replace('#', '');
    if (hex.length === 3) hex = hex.split('').map((c) => c + c).join('');
    const n = parseInt(hex || 'b9b9b9', 16);
    return `${(n >> 16) & 255},${(n >> 8) & 255},${n & 255}`;
  }

  // ---------- loop ----------
  function frame() {
    if (camTween) camTween();
    if (alpha >= 0.001) tick();
    // Keep the graph framed as it expands during settling; stop once it's at rest.
    if (autoFit && !camTween) { zoomToFit(false); if (alpha < 0.02) autoFit = false; }
    if (replayActive && !REDUCED) dirty = true; // keep the pulse animating during replay
    if (dirty) { draw(); dirty = false; }
    requestAnimationFrame(frame);
  }
  requestAnimationFrame(frame);

  // ---------- picking + interaction ----------
  function pick(sx, sy) {
    const [wx, wy] = toWorld(sx, sy);
    let best = null, bestD = Infinity;
    for (const n of nodes) {
      const dx = n.x - wx, dy = n.y - wy, d = dx * dx + dy * dy;
      const rr = Math.pow((radius(n) + 7) / cam.scale, 2);
      if (d < rr && d < bestD) { bestD = d; best = n; }
    }
    return best;
  }

  let drag = null, panning = null, moved = false;
  canvas.addEventListener('pointerdown', (ev) => {
    canvas.setPointerCapture(ev.pointerId);
    autoFit = false;
    moved = false;
    const rect = canvas.getBoundingClientRect();
    const sx = ev.clientX - rect.left, sy = ev.clientY - rect.top;
    const n = pick(sx, sy);
    if (n) { drag = { n, sx, sy }; n.fixed = true; }
    else { panning = { sx, sy, tx: cam.tx, ty: cam.ty }; }
  });
  canvas.addEventListener('pointermove', (ev) => {
    const rect = canvas.getBoundingClientRect();
    const sx = ev.clientX - rect.left, sy = ev.clientY - rect.top;
    if (drag) {
      moved = true;
      const [wx, wy] = toWorld(sx, sy);
      drag.n.x = wx; drag.n.y = wy; drag.n.vx = 0; drag.n.vy = 0;
      reheat(0.3); dirty = true; return;
    }
    if (panning) { moved = true; cam.tx = panning.tx + (sx - panning.sx); cam.ty = panning.ty + (sy - panning.sy); dirty = true; return; }
    const n = pick(sx, sy);
    // onHover gets canvas-relative coords (sx/sy), matching the tooltip's
    // position:absolute containing block — raw clientX/Y would offset it by
    // the rail + topbar.
    if (n !== hover) { hover = n; dirty = true; handlers.onHover && handlers.onHover(n, sx, sy); }
    else if (n) handlers.onHover && handlers.onHover(n, sx, sy);
    canvas.style.cursor = n ? 'pointer' : 'grab';
  });
  canvas.addEventListener('pointerup', (ev) => {
    const rect = canvas.getBoundingClientRect();
    const sx = ev.clientX - rect.left, sy = ev.clientY - rect.top;
    if (drag) { if (!moved) drag.n.fixed = false; const n = drag.n; drag = null; if (!moved) { handlers.onClick && handlers.onClick(n); } return; }
    if (panning) { const wasClick = !moved; panning = null; if (wasClick) { handlers.onBackground && handlers.onBackground(); } }
  });
  canvas.addEventListener('dblclick', (ev) => {
    const rect = canvas.getBoundingClientRect();
    const n = pick(ev.clientX - rect.left, ev.clientY - rect.top);
    if (n) handlers.onExpand && handlers.onExpand(n);
  });
  canvas.addEventListener('wheel', (ev) => {
    ev.preventDefault();
    autoFit = false;
    const rect = canvas.getBoundingClientRect();
    const sx = ev.clientX - rect.left, sy = ev.clientY - rect.top;
    const [wx, wy] = toWorld(sx, sy);
    const factor = Math.exp(-ev.deltaY * 0.0015);
    cam.scale = Math.max(0.05, Math.min(cam.scale * factor, 8));
    cam.tx = sx - wx * cam.scale; cam.ty = sy - wy * cam.scale;
    dirty = true;
  }, { passive: false });
  // Leaving the canvas ends the hover: clear the highlight and tell the app so it
  // cancels any pending (delayed) tooltip and hides a shown one — without this a
  // rest-timer could paint a stale tooltip after the cursor left onto the chrome.
  canvas.addEventListener('pointerleave', () => {
    if (hover !== null) { hover = null; dirty = true; }
    handlers.onHover && handlers.onHover(null);
  });

  return {
    setData, mergeData, focus, clearFocus, zoomToFit,
    setReplay, clearReplay, panToIds,
    getNode: (id) => byId.get(id),
    reheat,
    get count() { return nodes.length; },
  };
}
