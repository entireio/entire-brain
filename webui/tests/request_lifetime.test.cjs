const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const test = require('node:test');

// Run the shipped controller with deferred network replies and a minimal DOM.
// Canvas drawing is stubbed; state transitions and response handlers are real.
function fixture() {
  let source = fs.readFileSync(path.join(__dirname, '../dist/app.js'), 'utf8');
  source = source.replace(/^import .*;$/m, '');
  source = source.slice(0, source.indexOf('// ---------- boot (runs last'));
  const elements = new Map(), requests = [], timers = [], calls = [];
  function element(id) {
    if (!elements.has(id)) {
      const classes = new Set();
      elements.set(id, { id, value: '', textContent: '', innerHTML: '', style: {},
        classList: { add: x => classes.add(x), remove: x => classes.delete(x),
          toggle: (x, yes) => yes ? classes.add(x) : classes.delete(x), contains: x => classes.has(x) },
        setAttribute() {}, addEventListener() {}, querySelector: () => element(id + '/child'),
        querySelectorAll: () => [], focus() {} });
    }
    return elements.get(id);
  }
  const graph = {
    nodes: [], setData(nodes) { this.nodes = Array.from(nodes, n => n.id); calls.push(['set', ...this.nodes]); },
    mergeData(nodes) { calls.push(['merge', ...nodes.map(n => n.id)]); return nodes.length; },
    focus() {}, clearFocus() {}, clearReplay() {}, reheat() {}, zoomToFit() {},
    getNode: id => ({ id }), setReplay() { calls.push(['replay']); }
  };
  const context = vm.createContext({ console,
    document: { getElementById: element, addEventListener() {}, activeElement: { tagName: 'BODY' } },
    window: { addEventListener() {} }, createGraph: () => graph,
    colorForKind: () => '#fff', nodeColor: () => '#fff',
    setTimeout: fn => { timers.push(fn); return timers.length; }, clearTimeout() {},
    setInterval: () => 1, clearInterval() {},
    fetch: url => new Promise((resolve, reject) => requests.push({ url,
      resolve: data => resolve({ ok: true, json: async () => data }), reject }))
  });
  vm.runInContext(source, context);
  const run = code => vm.runInContext(code, context);
  run("currentFeature='sem'; ensureGraph()");
  return { run, requests, graph, calls, element, timers };
}

for (const oldFails of [false, true]) test(`latest slider request survives stale ${oldFails ? 'failure' : 'success'}`, async () => {
  const f = fixture();
  const old = f.run("currentLimit=300; loadFeatureGraph('sem', null, false)");
  const latest = f.run("currentLimit=500; loadFeatureGraph('sem', null, false)");
  f.requests[1].resolve({ nodes: [{ id: 'new' }] }); await latest;
  if (oldFails) f.requests[0].reject(new Error('stale failure'));
  else f.requests[0].resolve({ nodes: [{ id: 'old' }] });
  await old;
  assert.deepEqual(f.graph.nodes, ['new']);
  assert.notEqual(f.element('empty-title').textContent, 'Could not load');
});

test('new inspector survives old failures and repeated same-id successes', async () => {
  const f = fixture();
  const old = f.run("openNode('a')"), latest = f.run("openNode('b')");
  f.requests[1].resolve({ symbol: { id: 'b', name: 'new' } }); await latest;
  f.requests[0].reject(new Error('stale failure')); await old;
  assert.ok(!f.element('insp-body').innerHTML.includes('stale failure'));
  const first = f.run("openNode('b')"), second = f.run("openNode('b')");
  f.requests[3].resolve({ symbol: { id: 'b', name: 'newest' } }); await second;
  f.requests[2].resolve({ symbol: { id: 'b', name: 'older' } }); await first;
  assert.equal(f.element('insp-name').textContent, 'newest');
});

test('semantic expansion cannot merge into a newer feature', async () => {
  const f = fixture(), pending = f.run("expandNode({id:'a'})");
  const next = f.run("openFeature('facts')");
  f.requests[1].resolve({ nodes: [{ id: 'fact' }] }); await next;
  f.requests[0].resolve({ neighbors: [{ id: 'old-symbol' }] }); await pending;
  assert.ok(!f.calls.some(call => call[0] === 'merge'));
});

for (const replay of [false, true]) test(`empty ${replay ? 'replay' : 'feature'} clears graph and selection`, async () => {
  const f = fixture();
  f.graph.setData([{ id: 'old' }]); f.run("currentSel='old'");
  const pending = f.run(replay ? "startReplay('session')" : "loadFeatureGraph('sem', null, false)");
  f.requests[0].resolve({ nodes: [] }); await pending;
  assert.deepEqual(f.graph.nodes, []);
  assert.equal(f.run('currentSel'), null);
});

test('replay requests, timers and failures respect their lifetime', async () => {
  const f = fixture();
  const old = f.run("startReplay('old')"), latest = f.run("startReplay('new')");
  f.requests[1].resolve({ nodes: [{ id: 'new' }], session: { name: 'new' } }); await latest;
  f.requests[0].resolve({ nodes: [{ id: 'old' }], session: { name: 'old' } }); await old;
  assert.deepEqual(f.graph.nodes, ['new']);
  assert.equal(f.element('rp-title').textContent, 'new');
  f.run('showHub()'); f.timers.forEach(fn => fn());
  assert.ok(!f.calls.some(call => call[0] === 'replay'));
  const abandoned = f.run("startReplay('abandoned')"); f.run('showHub()');
  f.requests[2].reject(new Error('stale failure')); await abandoned;
  assert.notEqual(f.element('empty-title').textContent, 'Could not load replay');
});
