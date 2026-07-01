// app.js — wires the API to the graph, inspector, search, and ⌘K palette.
import { createGraph, colorForKind } from './graph.js';

const $ = (id) => document.getElementById(id);
const app = $('app');
let graph = null;
let currentSel = null;

async function api(path) {
  const res = await fetch(path, { headers: { Accept: 'application/json' } });
  if (!res.ok) throw new Error(`${path}: ${res.status}`);
  return res.json();
}
function debounce(fn, ms) { let t; return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); }; }
const esc = (s) => (s || '').replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));

// ---------- boot ----------
(async function boot() {
  graph = createGraph($('graph'), {
    onHover: showTooltip,
    onClick: (n) => openNode(n.id),
    onExpand: expandNode,
    onBackground: closeInspector,
  });

  try {
    const summary = await api('/api/summary');
    renderSummary(summary);
  } catch (e) { /* summary is best-effort */ }

  try {
    const g = await api('/api/graph');
    $('loading').classList.add('hidden');
    if (!g.nodes || g.nodes.length === 0) {
      showEmpty(g.warnings);
      return;
    }
    graph.setData(g.nodes, g.edges || []);
    $('results-label').textContent = `${g.nodes.length} symbols` + (g.truncated ? ` of ${g.total} — raise --limit` : '');
  } catch (e) {
    $('loading').classList.add('hidden');
    showEmpty([String(e.message || e)]);
  }
})();

function renderSummary(s) {
  $('brand-sub').textContent = [s.repo ? s.repo.split('/').pop() : '', s.branch].filter(Boolean).join(' · ') || 'viz';
  const c = s.counts || {};
  const parts = [
    ['symbols', c.symbols], ['relations', c.relations], ['files', c.files],
    ['facts', c.facts], ['history', c.history], ['sessions', c.sessions],
  ].filter(([, v]) => v > 0).map(([k, v]) => `<span>${k} <b>${v}</b></span>`);
  $('counts').innerHTML = parts.join('');
}

function showEmpty(warnings) {
  const box = $('empty');
  box.classList.remove('hidden');
  if (warnings && warnings.length) {
    $('empty-title').textContent = 'Nothing to show yet';
    $('empty-body').innerHTML = warnings.map(esc).join('<br>') + '<br><br>Run <code>entire brain refresh</code>, then reopen viz.';
  }
}

// ---------- tooltip ----------
const tip = $('tooltip');
function showTooltip(n, clientX, clientY) {
  if (!n) { tip.classList.remove('show'); return; }
  const file = n.file ? `<br><span class="k">${esc(n.file)}${n.line ? ':' + n.line : ''}</span>` : '';
  tip.innerHTML = `<span style="color:${colorForKind(n.kind)}">${esc(n.name || n.id)}</span> <span class="k">${esc(n.kind)}</span>${file}`;
  tip.style.left = clientX + 'px';
  tip.style.top = clientY + 'px';
  tip.classList.add('show');
}

// ---------- inspector ----------
async function openNode(id) {
  currentSel = id;
  graph.focus(id);
  app.classList.add('inspector-open');
  const base = graph.getNode(id) || { id };
  paintInspectorHead(base);
  $('insp-body').innerHTML = '<div class="empty-note">Loading…</div>';
  try {
    const d = await api('/api/node?id=' + encodeURIComponent(id));
    if (currentSel !== id) return;
    paintInspectorHead(d.symbol || base);
    paintInspectorBody(d);
  } catch (e) {
    $('insp-body').innerHTML = `<div class="empty-note">Could not load: ${esc(String(e.message || e))}</div>`;
  }
}

function paintInspectorHead(n) {
  const col = colorForKind(n.kind);
  const kind = $('insp-kind');
  kind.querySelector('.dot').style.background = col;
  kind.querySelector('span:last-child').textContent = n.kind || 'symbol';
  $('insp-name').textContent = n.name || n.id;
  $('insp-qual').textContent = n.qualified_name && n.qualified_name !== n.name ? n.qualified_name : '';
  $('insp-file').textContent = n.file ? n.file + (n.line ? ':' + n.line : '') : '';
}

function paintInspectorBody(d) {
  const sel = d.symbol || {};
  let html = '';
  if (sel.signature) html += `<div class="insp-sig">${esc(sel.signature)}</div>`;
  if (d.snippet && d.snippet.trim() && d.snippet.trim() !== (sel.signature || '').trim()) {
    html += `<div class="insp-h">Source</div><div class="insp-sig">${esc(d.snippet.slice(0, 1400))}</div>`;
  }
  const neighbors = d.neighbors || [];
  html += `<div class="insp-h">Neighbors · ${neighbors.length}</div>`;
  if (!neighbors.length) html += `<div class="empty-note">No linked symbols.</div>`;
  else {
    const relOf = (nid) => {
      for (const r of d.relations || []) {
        if (r.from === sel.id && r.to === nid) return '→ ' + r.type;
        if (r.to === sel.id && r.from === nid) return '← ' + r.type;
      }
      return '';
    };
    html += neighbors.map((nb) =>
      `<div class="neighbor" data-id="${esc(nb.id)}"><span class="dot" style="background:${colorForKind(nb.kind)}"></span>` +
      `<span class="n" title="${esc(nb.qualified_name || nb.name)}">${esc(nb.name || nb.id)}</span>` +
      `<span class="rel">${esc(relOf(nb.id))}</span></div>`).join('');
  }
  const body = $('insp-body');
  body.innerHTML = html;
  body.querySelectorAll('.neighbor').forEach((el) => el.addEventListener('click', () => openNode(el.dataset.id)));
}

async function expandNode(n) {
  try {
    const d = await api('/api/node?id=' + encodeURIComponent(n.id));
    const added = graph.mergeData(d.neighbors || [], d.relations || []);
    if (added > 0) graph.reheat(0.6);
  } catch (e) { /* best-effort */ }
}

function closeInspector() { app.classList.remove('inspector-open'); currentSel = null; graph && graph.clearFocus(); }
$('insp-close').addEventListener('click', closeInspector);
$('insp-focus').addEventListener('click', () => currentSel && graph.focus(currentSel));
$('insp-expand').addEventListener('click', () => { const n = currentSel && graph.getNode(currentSel); if (n) expandNode(n); });
$('fit').addEventListener('click', () => graph.zoomToFit());

// ---------- rail search ----------
const results = $('results');
const runSearch = debounce(async (q) => {
  if (!q.trim()) { results.innerHTML = ''; $('results-label').textContent = 'Graph'; $('legend').classList.remove('hidden'); return; }
  $('legend').classList.add('hidden');
  try {
    const r = await api('/api/search?q=' + encodeURIComponent(q));
    renderResults(results, r.hits || []);
    $('results-label').textContent = `${(r.hits || []).length} results`;
  } catch (e) { results.innerHTML = `<div class="empty-note" style="padding:8px 16px">search failed</div>`; }
}, 160);
$('search').addEventListener('input', (e) => runSearch(e.target.value));

function renderResults(container, hits) {
  if (!hits.length) { container.innerHTML = `<div class="empty-note" style="padding:8px 16px">No matches</div>`; return; }
  container.innerHTML = hits.map((h, i) => {
    const col = h.source === 'symbol' ? colorForKind(h.kind) : 'var(--text-disabled)';
    return `<button class="result" data-i="${i}"><span class="dot" style="background:${col}"></span>` +
      `<span class="name" title="${esc(h.text || h.title)}">${esc(h.title || h.text || h.id)}</span>` +
      `<span class="meta">${esc(h.source)}</span></button>`;
  }).join('');
  container.querySelectorAll('.result').forEach((el) => el.addEventListener('click', () => selectHit(hits[+el.dataset.i])));
}

function selectHit(h) {
  if (h.source === 'symbol' && graph.getNode(h.id)) { openNode(h.id); return; }
  if (h.source === 'symbol') { openNode(h.id); return; } // may 404 gracefully
  // non-graph memory hit: show its text in the inspector
  app.classList.add('inspector-open');
  currentSel = null;
  $('insp-kind').querySelector('.dot').style.background = 'var(--text-disabled)';
  $('insp-kind').querySelector('span:last-child').textContent = h.source;
  $('insp-name').textContent = h.title || h.source;
  $('insp-qual').textContent = h.path || '';
  $('insp-file').textContent = '';
  $('insp-body').innerHTML = `<div class="insp-sig">${esc(h.text || '')}</div>`;
}

// ---------- ⌘K palette ----------
const pal = $('palette'), palInput = $('palette-input'), palList = $('palette-list');
let palHits = [], palActive = 0;
function openPalette() { pal.classList.remove('hidden'); palInput.value = ''; palList.innerHTML = ''; palHits = []; palActive = 0; palInput.focus(); }
function closePalette() { pal.classList.add('hidden'); }
const runPalette = debounce(async (q) => {
  if (!q.trim()) { palList.innerHTML = ''; palHits = []; return; }
  try {
    const r = await api('/api/search?q=' + encodeURIComponent(q));
    palHits = r.hits || []; palActive = 0; renderPalette();
  } catch (e) { palList.innerHTML = `<div class="item"><span class="name">search failed</span></div>`; }
}, 140);
function renderPalette() {
  palList.innerHTML = palHits.map((h, i) => {
    const col = h.source === 'symbol' ? colorForKind(h.kind) : 'var(--text-disabled)';
    return `<div class="item ${i === palActive ? 'active' : ''}" data-i="${i}"><span class="dot" style="background:${col}"></span>` +
      `<span class="name">${esc(h.title || h.text || h.id)}</span><span class="src">${esc(h.source)}</span></div>`;
  }).join('');
  palList.querySelectorAll('.item').forEach((el) => el.addEventListener('click', () => { closePalette(); selectHit(palHits[+el.dataset.i]); }));
  const active = palList.querySelector('.item.active');
  active && active.scrollIntoView({ block: 'nearest' });
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
  if (e.key === 'Escape') { if (!pal.classList.contains('hidden')) return closePalette(); if (app.classList.contains('inspector-open')) return closeInspector(); }
  if (typing) return;
  if (e.key === '/') { e.preventDefault(); $('search').focus(); }
  else if (e.key === '0') { e.preventDefault(); graph && graph.zoomToFit(); }
});
