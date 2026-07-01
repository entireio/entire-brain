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

// ---------- boot ----------
(async function boot() {
  let summary;
  try { summary = await api('/api/summary'); } catch (e) { summary = { counts: {} }; }
  renderSummary(summary);
  renderHub(summary.counts || {});
  showHub();
})();

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
    const [lx, ly] = polar(R + 54, a);
    const anchor = Math.abs(hx) < 40 ? 'middle' : hx < 0 ? 'end' : 'start';
    if (n) g += `<text class="feat-count" x="${lx.toFixed(1)}" y="${(ly - 16).toFixed(1)}" text-anchor="${anchor}">${nf(n)}</text>`;
    g += `<text class="feat-label" x="${lx.toFixed(1)}" y="${ly.toFixed(1)}" text-anchor="${anchor}" fill="${f.hex}">${esc(f.label.toUpperCase())}</text>`;
    g += `<text class="feat-sub" x="${lx.toFixed(1)}" y="${(ly + 15).toFixed(1)}" text-anchor="${anchor}">${esc(f.sub)}</text>`;
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
function showHub() { setView('hub'); currentFeature = null; $('legend').classList.add('hidden'); clearSearch(); }
$('home').addEventListener('click', showHub);
$('back').addEventListener('click', showHub);

async function openFeature(key, focusId) {
  const f = featureByKey[key];
  if (!f) return;
  currentFeature = key;
  setView('graph');
  $('crumb-title').textContent = f.label;
  $('crumb-count').textContent = '';
  $('legend').classList.toggle('hidden', key !== 'sem');
  $('insp-expand').style.display = key === 'sem' ? '' : 'none';
  ensureGraph();
  $('loading').classList.remove('hidden'); $('empty').classList.add('hidden');
  try {
    const g = await api(key === 'sem' ? '/api/graph' : '/api/' + key);
    $('loading').classList.add('hidden');
    const nodes = g.nodes || [];
    gData = { byId: Object.fromEntries(nodes.map((n) => [n.id, n])), edges: g.edges || [] };
    if (!nodes.length) {
      $('empty').classList.remove('hidden');
      $('empty-title').textContent = 'Nothing here yet';
      $('empty-body').textContent = (g.warnings && g.warnings[0]) || `No ${f.label.toLowerCase()} yet.`;
      return;
    }
    graph.setData(nodes, g.edges || []);
    const shown = nodes.length, total = g.total || shown;
    $('crumb-count').textContent = total > shown ? `${nf(shown)} of ${nf(total)}` : nf(total);
    // Re-fit as the force sim settles — large graphs keep expanding past the first frame.
    [280, 900, 1800].forEach((t) => setTimeout(() => { if (graph && currentFeature === key) graph.zoomToFit(false); }, t));
    if (focusId) setTimeout(() => { if (currentFeature === key) openGraphNode(graph.getNode(focusId) || { id: focusId }); }, 650);
  } catch (e) {
    $('loading').classList.add('hidden'); $('empty').classList.remove('hidden');
    $('empty-title').textContent = 'Could not load'; $('empty-body').textContent = String(e.message || e);
  }
}

function openGraphNode(n) {
  if (!n) return;
  if (currentFeature === 'sem') { openNode(n.id); return; }
  openFeatureNode(n.id);
}

// ---------- tooltip ----------
const tip = $('tooltip');
function showTooltip(n, clientX, clientY) {
  if (!n) { tip.classList.remove('show'); return; }
  const sub = n.file ? `<br><span class="k">${esc(n.file)}${n.line ? ':' + n.line : ''}</span>` : (n.meta ? `<br><span class="k">${esc(n.meta)}</span>` : '');
  tip.innerHTML = `<span style="color:${nodeColor(n)}">${esc(n.name || n.id)}</span> <span class="k">${esc(n.group || n.kind || '')}</span>${sub}`;
  tip.style.left = clientX + 'px'; tip.style.top = clientY + 'px'; tip.classList.add('show');
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
  currentSel = id; graph.focus(id); app.classList.add('inspector-open');
  paintHead(n);
  const nb = [];
  for (const e of gData.edges) {
    if (e.from === id && gData.byId[e.to]) nb.push({ node: gData.byId[e.to], rel: e.type, dir: '→' });
    else if (e.to === id && gData.byId[e.from]) nb.push({ node: gData.byId[e.from], rel: e.type, dir: '←' });
  }
  let html = '';
  html += linkButton(n);
  if (n.text) html += `<div class="insp-sig">${esc(n.text)}</div>`;
  html += `<div class="insp-h">Connected · ${nb.length}</div>`;
  if (!nb.length) html += `<div class="empty-note">No links.</div>`;
  else html += nb.slice(0, 80).map((x) => `<div class="neighbor" data-id="${esc(x.node.id)}"><span class="dot" style="background:${nodeColor(x.node)}"></span><span class="n" title="${esc(x.node.text || x.node.name)}">${esc(x.node.name || x.node.id)}</span><span class="rel">${esc((x.dir || '') + ' ' + (x.rel || ''))}</span></div>`).join('');
  const body = $('insp-body'); body.innerHTML = html;
  body.querySelectorAll('.neighbor').forEach((el) => el.addEventListener('click', () => openFeatureNode(el.dataset.id)));
}

async function expandNode(n) { try { const d = await api('/api/node?id=' + encodeURIComponent(n.id)); if (graph.mergeData(d.neighbors || [], d.relations || []) > 0) graph.reheat(0.6); } catch (e) { /* best-effort */ } }
function closeInspector() { app.classList.remove('inspector-open'); currentSel = null; graph && graph.clearFocus(); }
$('insp-close').addEventListener('click', closeInspector);
$('insp-focus').addEventListener('click', () => currentSel && graph.focus(currentSel));
$('insp-expand').addEventListener('click', () => { if (currentFeature === 'sem' && currentSel) { const n = graph.getNode(currentSel); if (n) expandNode(n); } });
$('fit').addEventListener('click', () => graph && graph.zoomToFit());

// ---------- search (global) ----------
const results = $('results');
function clearSearch() { $('search').value = ''; results.innerHTML = ''; $('results-label').hidden = true; }
function hitColor(h) { if (h.source === 'symbol') return colorForKind(h.kind); const k = SOURCE_TO_FEATURE[h.source]; return (featureByKey[k] && featureByKey[k].hex) || 'var(--text-disabled)'; }
const runSearch = debounce(async (q) => {
  if (!q.trim()) { results.innerHTML = ''; $('results-label').hidden = true; return; }
  $('results-label').hidden = false;
  try { const r = await api('/api/search?q=' + encodeURIComponent(q)); renderResults(r.hits || []); $('results-label').textContent = `${(r.hits || []).length} results`; }
  catch (e) { results.innerHTML = `<div class="empty-note" style="padding:8px 16px">search failed</div>`; }
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
const runPalette = debounce(async (q) => {
  if (!q.trim()) { palList.innerHTML = ''; palHits = []; return; }
  try { const r = await api('/api/search?q=' + encodeURIComponent(q)); palHits = r.hits || []; palActive = 0; renderPalette(); }
  catch (e) { palList.innerHTML = `<div class="item"><span class="name">search failed</span></div>`; }
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
