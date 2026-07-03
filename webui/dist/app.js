// app.js — brain viz: Level 1 is a radial constellation of the brain's features;
// every feature (Semantic, Facts, Sessions, History, Docs) drills into a graph,
// and nodes carry real entire.io / source links.
import { createGraph, colorForKind, nodeColor } from './graph.js';

const $ = (id) => document.getElementById(id);
const app = $('app');
let graph = null, view = 'hub', currentSel = null, currentFeature = null, brainMeta = {};
let gData = { byId: {}, edges: [] };

async function api(path) {
  const res = await fetch(path, { headers: { Accept: 'application/json' } });
  if (!res.ok) throw new Error(`${path}: ${res.status}`);
  return res.json();
}
const debounce = (fn, ms) => { let t; return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); }; };
const esc = (s) => (s || '').replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
const nf = (n) => (n || 0).toLocaleString();

// ---------- markdown (PROSE only; dependency-free, CSP-safe) ----------
// XSS-safety is the load-bearing property: we escape ALL html FIRST via esc(),
// then apply a small, fixed markdown subset to the ESCAPED text — so every tag
// and attribute in the output is one we emit here, never passed through from the
// source. Links are scheme-allowlisted (http/https/mailto); there is no raw-html
// passthrough and no javascript:/data: urls. Use for prose (fact/doc/history/
// session summaries), NOT for source code (that stays escaped in .insp-sig).
// Self-check: mdToHtml('<img src=x onerror=alert(1)>') -> "&lt;img ...&gt;" text;
// mdToHtml('[x](javascript:alert(1))') -> the literal, un-linked, escaped text.
const mdSafeUrl = (u) => { u = (u || '').trim(); return /^(https?:\/\/|mailto:)[^\s]+$/i.test(u) ? u : null; };
function mdEmph(s) {
  return s
    .replace(/\*\*([^*]+?)\*\*/g, '<strong>$1</strong>')
    .replace(/__([^_]+?)__/g, '<strong>$1</strong>')
    .replace(/(^|[^\w*])\*([^*\n]+?)\*(?!\w)/g, '$1<em>$2</em>')
    .replace(/(^|[^\w_])_([^_\n]+?)_(?!\w)/g, '$1<em>$2</em>');
}
function mdInline(s) {
  const tokens = [];
  const stash = (html) => { tokens.push(html); return '\uE000' + (tokens.length - 1) + '\uE000'; };
  // Protect inline code and links from the emphasis pass (and, for links, keep
  // emphasis regexes from corrupting an href full of _ or * characters).
  s = s.replace(/`([^`]+)`/g, (_, c) => stash('<code>' + c + '</code>'));
  s = s.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (m, text, url) => {
    const u = mdSafeUrl(url);
    return u ? stash(`<a href="${u}" target="_blank" rel="noopener noreferrer">${mdEmph(text)}</a>`) : m;
  });
  s = mdEmph(s);
  // Unstash repeatedly: a link's text can itself contain a stashed code token, so
  // a single pass would leave the inner sentinel as literal glyphs. Loop until no
  // sentinel remains (guard-bounded — token content is safe, escaped HTML only).
  for (let guard = 0; /\uE000\d+\uE000/.test(s) && guard < 6; guard++) {
    s = s.replace(/\uE000(\d+)\uE000/g, (_, i) => (tokens[+i] != null ? tokens[+i] : ''));
  }
  return s;
}
function mdToHtml(src) {
  // Strip the private-use sentinel from the source first, so crafted prose can't
  // smuggle a placeholder into the stash/unstash machinery below.
  const lines = esc(String(src == null ? '' : src).replace(/\uE000/g, '')).split('\n');
  const out = [];
  let para = [];
  const flush = () => { if (para.length) { out.push('<p>' + para.map(mdInline).join('<br>') + '</p>'); para = []; } };
  for (let i = 0; i < lines.length;) {
    const line = lines[i];
    if (/^\s*```/.test(line)) { // fenced code block
      flush(); i++;
      const buf = [];
      while (i < lines.length && !/^\s*```/.test(lines[i])) { buf.push(lines[i]); i++; }
      i++; // consume the closing fence
      out.push('<pre><code>' + buf.join('\n') + '</code></pre>');
      continue;
    }
    const h = /^(#{1,6})\s+(.*)$/.exec(line);
    if (h) { flush(); out.push(`<h${h[1].length}>${mdInline(h[2].trim())}</h${h[1].length}>`); i++; continue; }
    if (/^\s*&gt;\s?/.test(line)) { // blockquote ('>' is '&gt;' after escaping)
      flush();
      const buf = [];
      while (i < lines.length && /^\s*&gt;\s?/.test(lines[i])) { buf.push(lines[i].replace(/^\s*&gt;\s?/, '')); i++; }
      out.push('<blockquote>' + buf.map(mdInline).join('<br>') + '</blockquote>');
      continue;
    }
    if (/^\s*[-*]\s+/.test(line)) { // unordered list
      flush();
      const buf = [];
      while (i < lines.length && /^\s*[-*]\s+/.test(lines[i])) { buf.push(lines[i].replace(/^\s*[-*]\s+/, '')); i++; }
      out.push('<ul>' + buf.map((t) => '<li>' + mdInline(t) + '</li>').join('') + '</ul>');
      continue;
    }
    if (/^\s*\d+\.\s+/.test(line)) { // ordered list
      flush();
      const buf = [];
      while (i < lines.length && /^\s*\d+\.\s+/.test(lines[i])) { buf.push(lines[i].replace(/^\s*\d+\.\s+/, '')); i++; }
      out.push('<ol>' + buf.map((t) => '<li>' + mdInline(t) + '</li>').join('') + '</ol>');
      continue;
    }
    if (line.trim() === '') { flush(); i++; continue; }
    para.push(line); i++;
  }
  flush();
  return out.join('');
}

// ---------- audio (synthesized via Web Audio — no external files, CSP-safe) ----------
let audioCtx = null, soundOn = true, lastHoverAt = 0;
function ensureAudio() {
  if (!audioCtx) { try { audioCtx = new (window.AudioContext || window.webkitAudioContext)(); } catch (e) { audioCtx = null; } }
  if (audioCtx && audioCtx.state === 'suspended') audioCtx.resume();
}
const SFX = {
  hover: { f: 920, to: 780, t: 0.028, g: 0.012, type: 'sine' },
  click: { f: 480, to: 300, t: 0.07, g: 0.05, type: 'triangle' },
  open: { f: 560, to: 760, t: 0.10, g: 0.045, type: 'sine' },
  back: { f: 380, to: 240, t: 0.08, g: 0.04, type: 'sine' },
  step: { f: 700, to: 900, t: 0.05, g: 0.035, type: 'sine' },
};
function sfx(name) {
  if (!soundOn || !audioCtx) return;
  const s = SFX[name] || SFX.click;
  const t0 = audioCtx.currentTime;
  const osc = audioCtx.createOscillator(), gain = audioCtx.createGain();
  osc.type = s.type;
  osc.frequency.setValueAtTime(s.f, t0);
  osc.frequency.exponentialRampToValueAtTime(Math.max(1, s.to), t0 + s.t);
  gain.gain.setValueAtTime(0.0001, t0);
  gain.gain.exponentialRampToValueAtTime(s.g, t0 + 0.006);
  gain.gain.exponentialRampToValueAtTime(0.0001, t0 + s.t);
  osc.connect(gain); gain.connect(audioCtx.destination);
  osc.start(t0); osc.stop(t0 + s.t + 0.03);
}
// Browsers need a user gesture before audio; wake the context on the first one.
window.addEventListener('pointerdown', ensureAudio, { once: true });
// Delegated hover + click ticks across all interactive chrome.
const HOVER_SEL = '.feature-node, button, .result, .neighbor, .insp-link, .tool, .rp-btn, a';
// .neighbor and .insp-replay trigger their own 'open'/replay sounds, so exclude
// them here to avoid a double tick.
const CLICK_SEL = '.feature-node, button, .result, .insp-link, .tool, .rp-btn';
document.addEventListener('pointerover', (e) => {
  if (!soundOn || !audioCtx) return;
  if (!(e.target.closest && e.target.closest(HOVER_SEL))) return;
  const now = audioCtx.currentTime;
  if (now - lastHoverAt < 0.045) return;
  lastHoverAt = now;
  sfx('hover');
});
document.addEventListener('click', (e) => {
  const el = e.target.closest && e.target.closest(CLICK_SEL);
  if (!el || el.id === 'sound-toggle' || el.classList.contains('insp-replay')) return;
  ensureAudio();
  sfx(el.id === 'back' || el.id === 'rp-close' ? 'back' : 'click');
});
(function initSoundToggle() {
  const btn = $('sound-toggle');
  if (!btn) return;
  btn.setAttribute('aria-pressed', soundOn ? 'true' : 'false');
  btn.addEventListener('click', () => {
    soundOn = !soundOn;
    btn.setAttribute('aria-pressed', soundOn ? 'true' : 'false');
    btn.title = soundOn ? 'Mute interface sounds' : 'Enable interface sounds';
    if (soundOn) { ensureAudio(); sfx('open'); }
  });
})();

const FEATURES = [
  { key: 'sem', label: 'Semantic', sub: 'functions · types · calls', hex: '#22d3ee', count: (c) => c.symbols, glyph: 'sem' },
  { key: 'facts', label: 'Facts', sub: 'decisions · gotchas · rules', hex: '#fbbf24', count: (c) => c.facts, glyph: 'facts' },
  { key: 'sessions', label: 'Sessions', sub: 'agents · turns · work', hex: '#818cf8', count: (c) => c.sessions, glyph: 'sessions' },
  { key: 'history', label: 'History', sub: 'decisions · learnings', hex: '#f25333', count: (c) => c.history, glyph: 'history' },
  { key: 'docs', label: 'Docs', sub: 'seed · guides · context', hex: '#34d399', count: (c) => c.docs, glyph: 'docs' },
];
const featureByKey = Object.fromEntries(FEATURES.map((f) => [f.key, f]));
const SOURCE_TO_FEATURE = { symbol: 'sem', fact: 'facts', facts: 'facts', doc: 'docs', docs: 'docs', history: 'history', session: 'sessions', sessions: 'sessions' };

// --- radial layout helpers ---
const polar = (r, deg) => { const a = (deg * Math.PI) / 180; return [r * Math.cos(a), r * Math.sin(a)]; };
function mulberry32(seed) { let t = seed >>> 0; return () => { t += 0x6d2b79f5; let r = Math.imul(t ^ (t >>> 15), 1 | t); r ^= r + Math.imul(r ^ (r >>> 7), 61 | r); return ((r ^ (r >>> 14)) >>> 0) / 4294967296; }; }
function dendrites(hx, hy, aDeg, color, count, rng) {
  const branches = Math.max(5, Math.min(4 + Math.round(Math.log10(count + 1) * 3), 11));
  let s = '';
  for (let b = 0; b < branches; b++) {
    const ba = aDeg + (rng() - 0.5) * 95;
    let px = hx, py = hy, pts = `${px.toFixed(1)},${py.toFixed(1)}`, dots = '';
    const steps = 2 + Math.floor(rng() * 2);
    for (let k = 0; k < steps; k++) {
      const [dx, dy] = polar(26 + rng() * 30, ba + (rng() - 0.5) * 26);
      px += dx; py += dy; pts += ` ${px.toFixed(1)},${py.toFixed(1)}`;
      const rr = k === steps - 1 ? 1.6 + rng() * 1.8 : 1.1 + rng();
      dots += `<circle cx="${px.toFixed(1)}" cy="${py.toFixed(1)}" r="${rr.toFixed(1)}" fill="${color}" opacity="${(0.45 + rng() * 0.45).toFixed(2)}"/>`;
    }
    s += `<polyline class="dendrite" points="${pts}" stroke="${color}"/>${dots}`;
  }
  return s;
}
function coreDots(rng) { let s = ''; for (let i = 0; i < 16; i++) { const [x, y] = polar(rng() * 48, rng() * 360); s += `<circle cx="${x.toFixed(1)}" cy="${y.toFixed(1)}" r="${(0.8 + rng() * 1.5).toFixed(1)}" fill="#f25333" opacity="${(0.25 + rng() * 0.5).toFixed(2)}"/>`; } return s; }

const GLYPHS = {
  sem: '<circle cx="7" cy="7" r="2.3"/><circle cx="17" cy="6" r="1.8"/><circle cx="15" cy="17" r="2"/><path d="M9 8l6-1M9 8l5 8" stroke="currentColor" stroke-width="1.3" fill="none"/>',
  facts: '<path d="M6 4h9l4 4v12H6z" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M9 11h7M9 14h7M9 8h3" stroke="currentColor" stroke-width="1.3"/>',
  sessions: '<rect x="4" y="6" width="16" height="12" rx="2" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M4 10h16" stroke="currentColor" stroke-width="1.3"/>',
  history: '<circle cx="12" cy="12" r="8" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M12 7v5l3 2" stroke="currentColor" stroke-width="1.4" fill="none"/>',
  docs: '<path d="M6 4h12v16H6z" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M9 8h6M9 11h6M9 14h4" stroke="currentColor" stroke-width="1.3"/>',
};

// boot() runs at the very end of this module — after every const/listener is
// initialized — so it can safely call into the search/inspector helpers.

function renderSummary(s) {
  brainMeta = { repo: s.repo || '', branch: s.branch || '' };
  const repo = s.repo ? s.repo.split('/').pop() : '';
  $('brand-sub').textContent = [repo, s.branch].filter(Boolean).join(' · ') || 'viz';
  const c = s.counts || {};
  const parts = [['symbols', c.symbols], ['relations', c.relations], ['facts', c.facts], ['history', c.history], ['sessions', c.sessions]]
    .filter(([, v]) => v > 0).map(([k, v]) => `<span>${k} <b>${nf(v)}</b></span>`);
  $('counts').innerHTML = parts.join('');
}

// Level 1 — a radial constellation: features orbit a central core, each a colored
// hub node with an icon, a count, radiating dendrites, and a label.
function renderHub(c) {
  const R = 250, N = FEATURES.length;
  const defs = `<defs><radialGradient id="coreGlow"><stop offset="0%" stop-color="rgba(242,83,51,0.30)"/><stop offset="55%" stop-color="rgba(242,83,51,0.06)"/><stop offset="100%" stop-color="rgba(0,0,0,0)"/></radialGradient></defs>`;
  let groups = '';
  FEATURES.forEach((f, i) => {
    const a = -90 + i * (360 / N);
    const [hx, hy] = polar(R, a);
    const rng = mulberry32(1009 + i * 131);
    const n = f.count(c) || 0;
    let g = `<line class="spoke" x1="0" y1="0" x2="${hx.toFixed(1)}" y2="${hy.toFixed(1)}"/>`;
    g += dendrites(hx, hy, a, f.hex, n, rng);
    g += `<circle class="hub-halo" cx="${hx.toFixed(1)}" cy="${hy.toFixed(1)}" r="30" fill="${f.hex}"/>`;
    g += `<circle class="hub-ring" cx="${hx.toFixed(1)}" cy="${hy.toFixed(1)}" r="22" fill="var(--surface-raised)" stroke="${f.hex}" stroke-width="2"/>`;
    g += `<svg x="${(hx - 11).toFixed(1)}" y="${(hy - 11).toFixed(1)}" width="22" height="22" viewBox="0 0 24 24" style="color:${f.hex}">${GLYPHS[f.glyph]}</svg>`;
    // Label block sits outward on the spoke, anchored by direction (top = centered,
    // left = right-aligned, right = left-aligned). Title first, then the count
    // underneath it, then the tagline — stacked away from the node.
    const [lx, ly] = polar(R + 60, a);
    const anchor = Math.abs(hx) < 60 ? 'middle' : hx < 0 ? 'end' : 'start';
    g += `<text class="feat-label" x="${lx.toFixed(1)}" y="${ly.toFixed(1)}" text-anchor="${anchor}" fill="${f.hex}">${esc(f.label.toUpperCase())}</text>`;
    if (n) g += `<text class="feat-count" x="${lx.toFixed(1)}" y="${(ly + 19).toFixed(1)}" text-anchor="${anchor}">${nf(n)}</text>`;
    groups += `<g class="feature-node" data-key="${f.key}" role="button" tabindex="0" aria-label="${esc(f.label)}">${g}</g>`;
  });
  const repo = (brainMeta.repo || '').split('/').pop();
  const core = `<g class="hub-core"><circle r="95" fill="url(#coreGlow)"/>${coreDots(mulberry32(7))}<text class="core-title" y="-1">entire brain</text><text class="core-sub" y="17">${esc([repo, brainMeta.branch].filter(Boolean).join(' · '))}</text></g>`;
  $('hub-stage').innerHTML = `<svg viewBox="-520 -430 1040 860" preserveAspectRatio="xMidYMid meet" role="group" aria-label="Brain features">${defs}${core}${groups}</svg>`;
  $('hub-stage').querySelectorAll('.feature-node').forEach((el) => {
    el.addEventListener('click', () => openFeature(el.dataset.key));
    el.addEventListener('keydown', (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); openFeature(el.dataset.key); } });
  });
}

// ---------- view routing: hub <-> graph (every feature is a graph) ----------
function ensureGraph() {
  if (graph) return;
  graph = createGraph($('graph'), {
    onHover: showTooltip,
    onClick: (n) => openGraphNode(n),
    onExpand: (n) => { if (currentFeature === 'sem') expandNode(n); },
    onBackground: closeInspector,
  });
}
function setView(v) {
  view = v;
  $('hub').classList.toggle('hidden', v !== 'hub');
  $('graphview').classList.toggle('hidden', v !== 'graph');
  $('topbar').classList.toggle('hidden', v === 'hub');
  if (v === 'hub') closeInspector();
}
function showHub() { stopReplay(); setView('hub'); currentFeature = null; $('legend').classList.add('hidden'); $('node-ctl').classList.add('hidden'); clearSearch(); }
$('home').addEventListener('click', showHub);
$('back').addEventListener('click', showHub);

const NODE_SLIDER_MAX = 300000; // safety ceiling only; slider reaches each feature's true total
let currentLimit = 0; // 0 = server default; set by the node-count slider

async function openFeature(key, focusId) {
  const f = featureByKey[key];
  if (!f) return;
  stopReplay();
  currentFeature = key;
  currentLimit = 0;
  setView('graph');
  $('crumb-title').textContent = f.label;
  $('crumb-count').textContent = '';
  $('node-ctl').classList.add('hidden');
  $('legend').classList.toggle('hidden', key !== 'sem');
  $('insp-expand').style.display = key === 'sem' ? '' : 'none';
  ensureGraph();
  await loadFeatureGraph(key, focusId, true);
}

// Fetch (or re-fetch, when the slider changes) the current feature graph.
async function loadFeatureGraph(key, focusId, fit) {
  const f = featureByKey[key];
  $('loading').classList.remove('hidden'); $('empty').classList.add('hidden');
  try {
    const base = key === 'sem' ? '/api/graph' : '/api/' + key;
    const g = await api(base + (currentLimit ? '?limit=' + currentLimit : ''));
    if (currentFeature !== key) return;
    $('loading').classList.add('hidden');
    const nodes = g.nodes || [];
    gData = { byId: Object.fromEntries(nodes.map((n) => [n.id, n])), edges: g.edges || [] };
    const total = g.total || nodes.length;
    if (!nodes.length) {
      $('empty').classList.remove('hidden');
      $('empty-title').textContent = 'Nothing here yet';
      $('empty-body').textContent = (g.warnings && g.warnings[0]) || `No ${f.label.toLowerCase()} yet.`;
      setupSlider(nodes.length, total);
      return;
    }
    graph.setData(nodes, g.edges || []);
    const shown = nodes.length;
    $('crumb-count').textContent = total > shown ? `${nf(shown)} of ${nf(total)}` : nf(total);
    setupSlider(shown, total);
    if (fit) [280, 900, 1800].forEach((t) => setTimeout(() => { if (graph && currentFeature === key) graph.zoomToFit(false); }, t));
    if (focusId) setTimeout(() => { if (currentFeature === key) openGraphNode(graph.getNode(focusId) || { id: focusId }); }, 650);
  } catch (e) {
    if (currentFeature !== key) return; // a stale failure must not clobber the active view
    $('loading').classList.add('hidden'); $('empty').classList.remove('hidden');
    $('empty-title').textContent = 'Could not load'; $('empty-body').textContent = String(e.message || e);
  }
}

// Configure the node-count slider for the current feature: range 50..min(total,cap).
function setupSlider(shown, total) {
  const ctl = $('node-ctl'), sl = $('node-slider');
  const max = Math.min(total, NODE_SLIDER_MAX);
  if (total <= 60 || max <= 50) { ctl.classList.add('hidden'); return; }
  sl.min = 50; sl.max = max; sl.step = max > 1000 ? 50 : 10;
  sl.value = String(currentLimit || Math.min(shown, max));
  $('node-slider-val').textContent = total > shown ? `${nf(shown)} / ${nf(total)}` : nf(shown);
  ctl.classList.remove('hidden');
}
// The debounce can fire after the user has already left the graph view (back
// to hub, or into replay) — bail instead of fetching a bogus /api/null.
const onSliderChange = debounce((v) => { if (!featureByKey[currentFeature]) return; currentLimit = v; loadFeatureGraph(currentFeature, null, true); }, 260);
$('node-slider').addEventListener('input', (e) => {
  const v = parseInt(e.target.value, 10) || 50;
  $('node-slider-val').textContent = nf(v);
  onSliderChange(v);
});

function openGraphNode(n) {
  if (!n) return;
  if (currentFeature === 'sem' || currentFeature === 'replay') { openNode(n.id); return; }
  openFeatureNode(n.id);
}

// ---------- tooltip ----------
const tip = $('tooltip');
let lastTipId = null;
function showTooltip(n, x, y) {
  // x/y are canvas-relative (see graph.js onHover) — the tooltip is absolutely
  // positioned inside #graphview, whose origin matches the canvas.
  if (!n) { tip.classList.remove('show'); lastTipId = null; return; }
  if (n.id !== lastTipId) { lastTipId = n.id; sfx('hover'); }
  const sub = n.file ? `<br><span class="k">${esc(n.file)}${n.line ? ':' + n.line : ''}</span>` : (n.meta ? `<br><span class="k">${esc(n.meta)}</span>` : '');
  tip.innerHTML = `<span style="color:${nodeColor(n)}">${esc(n.name || n.id)}</span> <span class="k">${esc(n.group || n.kind || '')}</span>${sub}`;
  tip.style.left = x + 'px'; tip.style.top = y + 'px'; tip.classList.add('show');
}

// ---------- inspector ----------
function linkButton(o) {
  if (!o || !o.link) return '';
  return `<a class="insp-link" href="${esc(o.link)}" target="_blank" rel="noopener noreferrer">${esc(o.link_label || o.linkLabel || 'Open in Entire')} &#8599;</a>`;
}
function paintHead(n) {
  const col = n.color || colorForKind(n.kind), kind = $('insp-kind');
  kind.querySelector('.dot').style.background = col;
  kind.querySelector('span:last-child').textContent = n.group || n.kind || 'item';
  $('insp-name').textContent = n.name || n.id;
  $('insp-qual').textContent = n.meta || (n.qualified_name && n.qualified_name !== n.name ? n.qualified_name : '') || '';
  $('insp-file').textContent = n.file ? n.file + (n.line ? ':' + n.line : '') : '';
}

// Semantic symbol: rich detail via /api/node (signature, snippet, neighbors, link).
async function openNode(id) {
  sfx('open');
  currentSel = id; ensureGraph(); graph.focus(id); app.classList.add('inspector-open');
  paintHead(graph.getNode(id) || { id });
  $('insp-body').innerHTML = '<div class="empty-note">Loading…</div>';
  try {
    const d = await api('/api/node?id=' + encodeURIComponent(id));
    if (currentSel !== id) return;
    paintHead(d.symbol || { id });
    let html = '';
    if (d.link) html += linkButton({ link: d.link, link_label: d.link_label });
    if (d.symbol && d.symbol.signature) html += `<div class="insp-sig">${esc(d.symbol.signature)}</div>`;
    if (d.snippet && d.snippet.trim() && d.snippet.trim() !== ((d.symbol && d.symbol.signature) || '').trim()) html += `<div class="insp-h">Source</div><div class="insp-sig">${esc(d.snippet.slice(0, 1400))}</div>`;
    const nb = d.neighbors || [];
    html += `<div class="insp-h">Neighbors · ${nb.length}</div>`;
    if (!nb.length) html += `<div class="empty-note">No linked symbols.</div>`;
    else {
      const relOf = (nid) => { for (const r of d.relations || []) { if (r.from === (d.symbol || {}).id && r.to === nid) return '→ ' + r.type; if (r.to === (d.symbol || {}).id && r.from === nid) return '← ' + r.type; } return ''; };
      html += nb.map((x) => `<div class="neighbor" data-id="${esc(x.id)}"><span class="dot" style="background:${nodeColor(x)}"></span><span class="n" title="${esc(x.qualified_name || x.name)}">${esc(x.name || x.id)}</span><span class="rel">${esc(relOf(x.id))}</span></div>`).join('');
    }
    const body = $('insp-body'); body.innerHTML = html;
    body.querySelectorAll('.neighbor').forEach((el) => el.addEventListener('click', () => openNode(el.dataset.id)));
  } catch (e) { $('insp-body').innerHTML = `<div class="empty-note">Could not load: ${esc(String(e.message || e))}</div>`; }
}

// Feature node (fact/session/history/doc): detail comes from the loaded graph data.
function openFeatureNode(id) {
  const n = gData.byId[id];
  if (!n) return;
  sfx('open');
  currentSel = id; graph.focus(id); app.classList.add('inspector-open');
  paintHead(n);
  const nb = [];
  for (const e of gData.edges) {
    if (e.from === id && gData.byId[e.to]) nb.push({ node: gData.byId[e.to], rel: e.type, dir: '→' });
    else if (e.to === id && gData.byId[e.from]) nb.push({ node: gData.byId[e.from], rel: e.type, dir: '←' });
  }
  const isSession = currentFeature === 'sessions';
  let html = '';
  html += linkButton(n);
  if (isSession) html += `<button class="insp-replay" data-replay="${esc(id)}">&#9654;&#65038; Replay on graph</button>`;
  // Sessions: surface the date · files · checkpoints meta clearly in the body so
  // the sidebar is never near-blank (it's otherwise only in the tiny qualifier).
  if (isSession && n.meta) html += `<div class="insp-meta">${esc(n.meta)}</div>`;
  // Prose summaries render as markdown; source code stays escaped in .insp-sig.
  if (n.text) html += `<div class="insp-md">${mdToHtml(n.text)}</div>`;
  else if (isSession) html += `<div class="empty-note">No summary recorded for this session.</div>`;
  html += `<div class="insp-h">Connected · ${nb.length}</div>`;
  if (!nb.length) html += `<div class="empty-note">${isSession ? 'This session recorded no linked symbols.' : 'No links.'}</div>`;
  else html += nb.slice(0, 80).map((x) => `<div class="neighbor" data-id="${esc(x.node.id)}"><span class="dot" style="background:${nodeColor(x.node)}"></span><span class="n" title="${esc(x.node.text || x.node.name)}">${esc(x.node.name || x.node.id)}</span><span class="rel">${esc((x.dir || '') + ' ' + (x.rel || ''))}</span></div>`).join('');
  const body = $('insp-body'); body.innerHTML = html;
  body.querySelectorAll('.insp-replay').forEach((el) => el.addEventListener('click', () => startReplay(el.dataset.replay)));
  body.querySelectorAll('.neighbor').forEach((el) => el.addEventListener('click', () => openFeatureNode(el.dataset.id)));
}

async function expandNode(n) { try { const d = await api('/api/node?id=' + encodeURIComponent(n.id)); if (graph.mergeData(d.neighbors || [], d.relations || []) > 0) graph.reheat(0.6); } catch (e) { /* best-effort */ } }
function closeInspector() { app.classList.remove('inspector-open'); currentSel = null; graph && graph.clearFocus(); }
$('insp-close').addEventListener('click', closeInspector);
$('insp-focus').addEventListener('click', () => currentSel && graph.focus(currentSel));
$('insp-expand').addEventListener('click', () => { if (currentFeature === 'sem' && currentSel) { const n = graph.getNode(currentSel); if (n) expandNode(n); } });
$('fit').addEventListener('click', () => graph && graph.zoomToFit());

// ---------- session replay ----------
// Fetch a session's touched-file symbols as a focused subgraph, then walk the
// files one step at a time — lighting up each file's symbols with a growing trail.
let replay = null;
const RP_PLAY = '▶', RP_PAUSE = '⏸';

function stopReplay() {
  if (replay) { clearInterval(replay.timer); replay = null; }
  graph && graph.clearReplay();
  $('replaybar').classList.add('hidden');
}

async function startReplay(sessionId) {
  ensureGraph();
  stopReplay();
  setView('graph');
  currentFeature = 'replay';
  $('legend').classList.add('hidden');
  $('node-ctl').classList.add('hidden');
  $('insp-expand').style.display = 'none';
  $('crumb-title').textContent = 'Replay';
  $('crumb-count').textContent = '';
  closeInspector();
  $('loading').classList.remove('hidden'); $('empty').classList.add('hidden');
  sfx('open');
  try {
    const d = await api('/api/session/replay?id=' + encodeURIComponent(sessionId));
    if (currentFeature !== 'replay') return;
    $('loading').classList.add('hidden');
    const nodes = d.nodes || [];
    gData = { byId: Object.fromEntries(nodes.map((n) => [n.id, n])), edges: d.edges || [] };
    if (!nodes.length) {
      $('empty').classList.remove('hidden');
      $('empty-title').textContent = 'Nothing to replay';
      $('empty-body').textContent = (d.warnings && d.warnings[0]) || 'This session touched no indexed symbols.';
      return;
    }
    graph.setData(nodes, d.edges || []); // autoFit frames the whole subgraph as it settles
    replay = { steps: d.steps || [], idx: -1, playing: false, timer: null, name: (d.session && d.session.name) || 'session' };
    $('rp-title').textContent = replay.name;
    const scrub = $('rp-scrub'); scrub.min = 0; scrub.max = Math.max(0, replay.steps.length - 1); scrub.value = 0;
    $('replaybar').classList.remove('hidden');
    // Let it settle + frame the whole graph first, then start stepping through it
    // in place (no per-step camera jumps — that was the disorienting part).
    setTimeout(() => { if (currentFeature === 'replay' && replay) { graph.zoomToFit(false); replaySeek(0); replayPlay(); } }, 1100);
  } catch (e) {
    $('loading').classList.add('hidden'); $('empty').classList.remove('hidden');
    $('empty-title').textContent = 'Could not load replay'; $('empty-body').textContent = String(e.message || e);
  }
}

function replayApply() {
  if (!replay) return;
  const step = replay.steps[replay.idx];
  const active = (step && step.ids) || [];
  const visited = new Set();
  for (let i = 0; i <= replay.idx; i++) (replay.steps[i].ids || []).forEach((id) => visited.add(id));
  graph.setReplay(active, visited);
  $('rp-step').textContent = `${replay.idx + 1} / ${replay.steps.length}`;
  $('rp-file').textContent = step ? `${step.file}  ·  ${step.ids.length ? step.ids.length + ' symbols' : 'no symbols'}` : '';
  $('rp-scrub').value = String(replay.idx);
  sfx('step');
}
function replaySeek(i) { if (!replay) return; replay.idx = Math.max(0, Math.min(i, replay.steps.length - 1)); replayApply(); }
function replayPlay() {
  if (!replay) return;
  replay.playing = true; $('rp-play').textContent = RP_PAUSE;
  clearInterval(replay.timer);
  replay.timer = setInterval(() => {
    if (!replay || replay.idx >= replay.steps.length - 1) { replayPause(); return; }
    replaySeek(replay.idx + 1);
  }, 1700);
}
function replayPause() { if (!replay) return; replay.playing = false; clearInterval(replay.timer); replay.timer = null; $('rp-play').textContent = RP_PLAY; }
function replayToggle() {
  if (!replay) return;
  if (replay.playing) { replayPause(); return; }
  if (replay.idx >= replay.steps.length - 1) replaySeek(0);
  replayPlay();
}
$('rp-play').addEventListener('click', replayToggle);
$('rp-prev').addEventListener('click', () => { replayPause(); replaySeek((replay ? replay.idx : 0) - 1); });
$('rp-next').addEventListener('click', () => { replayPause(); replaySeek((replay ? replay.idx : 0) + 1); });
$('rp-scrub').addEventListener('input', (e) => { replayPause(); replaySeek(parseInt(e.target.value, 10) || 0); });
$('rp-close').addEventListener('click', () => { stopReplay(); openFeature('sessions'); });

// ---------- search (global) ----------
const results = $('results');
function clearSearch() { $('search').value = ''; results.innerHTML = ''; $('results-label').hidden = true; }
function hitColor(h) { if (h.source === 'symbol') return colorForKind(h.kind); const k = SOURCE_TO_FEATURE[h.source]; return (featureByKey[k] && featureByKey[k].hex) || 'var(--text-disabled)'; }
let searchSeq = 0;
const runSearch = debounce(async (q) => {
  const seq = ++searchSeq; // bump first so clearing also invalidates an in-flight fetch
  if (!q.trim()) { results.innerHTML = ''; $('results-label').hidden = true; return; }
  $('results-label').hidden = false;
  try { const r = await api('/api/search?q=' + encodeURIComponent(q)); if (seq !== searchSeq) return; renderResults(r.hits || []); $('results-label').textContent = `${(r.hits || []).length} results`; }
  catch (e) { if (seq === searchSeq) results.innerHTML = `<div class="empty-note" style="padding:8px 16px">search failed</div>`; }
}, 160);
$('search').addEventListener('input', (e) => runSearch(e.target.value));

function renderResults(hits) {
  if (!hits.length) { results.innerHTML = `<div class="empty-note" style="padding:8px 16px">No matches</div>`; return; }
  results.innerHTML = hits.map((h, i) => `<button class="result" data-i="${i}"><span class="dot" style="background:${hitColor(h)}"></span><span class="name" title="${esc(h.text || h.title)}">${esc(h.title || h.text || h.id)}</span><span class="meta">${esc(h.source)}</span></button>`).join('');
  results.querySelectorAll('.result').forEach((el) => el.addEventListener('click', () => selectHit(hits[+el.dataset.i])));
}
function selectHit(h) {
  const key = SOURCE_TO_FEATURE[h.source] || 'sem';
  openFeature(key, h.id);
}

// ---------- command palette ----------
const pal = $('palette'), palInput = $('palette-input'), palList = $('palette-list');
let palHits = [], palActive = 0;
function openPalette() { pal.classList.remove('hidden'); palInput.value = ''; palList.innerHTML = ''; palHits = []; palActive = 0; palInput.focus(); }
function closePalette() { pal.classList.add('hidden'); }
let palSeq = 0;
const runPalette = debounce(async (q) => {
  const seq = ++palSeq; // bump first so clearing also invalidates an in-flight fetch
  if (!q.trim()) { palList.innerHTML = ''; palHits = []; return; }
  try { const r = await api('/api/search?q=' + encodeURIComponent(q)); if (seq !== palSeq) return; palHits = r.hits || []; palActive = 0; renderPalette(); }
  catch (e) { if (seq === palSeq) palList.innerHTML = `<div class="item"><span class="name">search failed</span></div>`; }
}, 140);
function renderPalette() {
  palList.innerHTML = palHits.map((h, i) => `<div class="item ${i === palActive ? 'active' : ''}" data-i="${i}"><span class="dot" style="background:${hitColor(h)}"></span><span class="name">${esc(h.title || h.text || h.id)}</span><span class="src">${esc(h.source)}</span></div>`).join('');
  palList.querySelectorAll('.item').forEach((el) => el.addEventListener('click', () => { closePalette(); selectHit(palHits[+el.dataset.i]); }));
  const active = palList.querySelector('.item.active'); active && active.scrollIntoView({ block: 'nearest' });
}
palInput.addEventListener('input', (e) => runPalette(e.target.value));
palInput.addEventListener('keydown', (e) => {
  if (e.key === 'ArrowDown') { e.preventDefault(); palActive = Math.min(palActive + 1, palHits.length - 1); renderPalette(); }
  else if (e.key === 'ArrowUp') { e.preventDefault(); palActive = Math.max(palActive - 1, 0); renderPalette(); }
  else if (e.key === 'Enter') { e.preventDefault(); if (palHits[palActive]) { closePalette(); selectHit(palHits[palActive]); } }
});
pal.addEventListener('click', (e) => { if (e.target === pal) closePalette(); });

// ---------- global keys ----------
window.addEventListener('keydown', (e) => {
  const typing = /^(INPUT|TEXTAREA)$/.test(document.activeElement.tagName);
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); pal.classList.contains('hidden') ? openPalette() : closePalette(); return; }
  if (e.key === 'Escape') { if (!pal.classList.contains('hidden')) return closePalette(); if (app.classList.contains('inspector-open')) return closeInspector(); if (view !== 'hub') return showHub(); }
  if (typing) return;
  if (e.key === '/') { e.preventDefault(); $('search').focus(); }
  else if (e.key === '0' && view === 'graph') { e.preventDefault(); graph && graph.zoomToFit(); }
});

// ---------- boot (runs last, after all consts + listeners are initialized) ----------
(function boot() {
  // Paint the hub shell immediately so the page is never blank — the summary
  // fetch can take a few seconds on large brains. Counts + repo/branch fill in
  // when it resolves.
  renderHub({});
  showHub();
  api('/api/summary')
    .then((summary) => { renderSummary(summary); renderHub(summary.counts || {}); })
    .catch(() => { /* keep the countless hub */ });
})();
