const { test, expect } = require('@playwright/test');
const { spawn, execFileSync } = require('node:child_process');
const { mkdtempSync, mkdirSync, rmSync } = require('node:fs');
const { tmpdir } = require('node:os');
const path = require('node:path');
const { once } = require('node:events');

let server, root, origin;
const browserErrors = new WeakMap();

test.beforeAll(async () => {
  if (!process.env.BRAIN_VIZ_BINARY) throw new Error('Build entire-brain and set BRAIN_VIZ_BINARY to its absolute path');
  root = mkdtempSync(path.join(tmpdir(), 'brain-browser-'));
  const repo = path.join(root, 'repo');
  mkdirSync(repo);
  const env = { ...process.env, HOME: root, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: path.join(root, 'gitconfig'), GIT_CONFIG_COUNT: '0' };
  for (const kind of ['DATA', 'CONFIG', 'STATE', 'CACHE']) env[`ENTIRE_PLUGIN_${kind}_DIR`] = path.join(root, kind.toLowerCase());
  execFileSync('git', ['init', '-q', '-b', 'main', repo], { env });
  execFileSync('git', ['-C', repo, '-c', 'user.name=Browser test', '-c', 'user.email=browser@example.invalid', 'commit', '--allow-empty', '-qm', 'fixture'], { env });
  server = spawn(process.env.BRAIN_VIZ_BINARY, ['viz', repo, '--no-open', '--port', '0'], { env, stdio: ['ignore', 'pipe', 'pipe'] });
  origin = await new Promise((resolve, reject) => {
    let output = '';
    const timer = setTimeout(() => reject(new Error(`viz did not start: ${output}`)), 15000);
    server.once('error', error => { clearTimeout(timer); reject(error); });
    server.once('exit', code => { clearTimeout(timer); reject(new Error(`viz exited ${code}: ${output}`)); });
    const read = chunk => {
      output += chunk;
      const match = output.match(/http:\/\/127\.0\.0\.1:\d+/);
      if (match) { clearTimeout(timer); resolve(match[0]); }
    };
    server.stdout.on('data', read);
    server.stderr.on('data', read);
  });
});

test.afterAll(async () => {
  try {
    if (server && server.exitCode === null && server.signalCode === null) {
      const exited = once(server, 'exit');
      const timer = setTimeout(() => server.kill('SIGKILL'), 3000);
      try {
        server.kill('SIGTERM');
        await exited;
      } finally {
        clearTimeout(timer);
      }
    }
  } finally {
    if (root) rmSync(root, { recursive: true, force: true });
  }
});

test.beforeEach(async ({ page }) => {
  const errors = [];
  browserErrors.set(page, errors);
  page.on('pageerror', error => errors.push(error.message));
  await page.route('**/*', route => {
    if (new URL(route.request().url()).origin !== origin) {
      errors.push(`unexpected outbound request: ${route.request().url()}`);
      return route.abort();
    }
    return route.continue();
  });
});

test.afterEach(async ({ page }) => expect(browserErrors.get(page)).toEqual([]));

test('embedded assets boot and real empty-brain API renders', async ({ page }) => {
  const summary = page.waitForResponse(response => response.url() === `${origin}/api/summary`);
  await page.goto(origin);
  expect((await summary).ok()).toBeTruthy();
  await expect(page.locator('#hub-stage .feature-node')).toHaveCount(5);
  await expect(page.locator('#hub-stage > svg')).toBeVisible();
  expect(await page.locator('#app').evaluate(element => getComputedStyle(element).display)).toBe('grid');
  const graphResponse = page.waitForResponse(response => response.url().startsWith(`${origin}/api/graph`));
  await page.locator('[data-key="sem"] .hub-ring').click();
  expect((await graphResponse).ok()).toBeTruthy();
  await expect(page.locator('#empty-title')).toHaveText('Nothing here yet');
  await expect(page.locator('#loading')).toBeHidden();
  await expect(page.locator('#graph')).toBeVisible();
  await page.locator('#back').click();
  await expect(page.locator('#hub-stage > svg')).toBeVisible();
});

test('late graph responses cannot replace the active view, and empty results clear the canvas', async ({ page }) => {
  const nodes = [{ id: 'fact:a', name: 'Current fact', kind: 'fact', color: '#ff0000' }];
  let delayed;
  const requested = new Promise(resolve => { delayed = resolve; });
  let release;
  const held = new Promise(resolve => { release = resolve; });
  await page.route('**/api/facts*', async route => {
    if (new URL(route.request().url()).searchParams.get('limit') === '50') {
      delayed();
      await held;
      return route.fulfill({ json: { nodes: [{ id: 'old', name: 'Stale fact', color: '#0000ff' }], edges: [], total: 100 } });
    }
    return route.fulfill({ json: { nodes, edges: [], total: 100 } });
  });
  await page.route('**/api/docs*', route => route.fulfill({ json: { nodes: [], edges: [] } }));
  await page.goto(origin);
  await page.locator('[data-key="facts"] .hub-ring').click();
  await expect(page.locator('#crumb-title')).toHaveText('Facts');
  const redPixels = () => page.locator('#graph').evaluate(canvas => {
    const pixels = canvas.getContext('2d').getImageData(0, 0, canvas.width, canvas.height).data;
    let painted = 0;
    for (let i = 0; i < pixels.length; i += 4) {
      if (pixels[i] > 180 && pixels[i + 1] < 100 && pixels[i + 2] < 100 && pixels[i + 3] > 100) painted++;
    }
    return painted;
  });
  await expect.poll(redPixels).toBeGreaterThan(0);
  await page.locator('#node-slider').focus();
  await page.locator('#node-slider').press('Home');
  await requested;
  const latest = page.waitForResponse(response => response.url() === `${origin}/api/facts?limit=100`);
  await page.locator('#node-slider').press('End');
  await (await latest).finished();
  await expect(page.locator('#node-slider')).toHaveValue('100');
  const delivered = page.waitForResponse(response => response.url() === `${origin}/api/facts?limit=50`);
  release();
  await (await delivered).finished();
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  expect(await redPixels()).toBeGreaterThan(0);
  await expect(page.locator('#crumb-title')).toHaveText('Facts');
  await expect(page.locator('#empty')).toBeHidden();
  await page.locator('#back').click();
  await page.locator('[data-key="docs"] .hub-ring').click();
  await expect(page.locator('#empty-title')).toHaveText('Nothing here yet');
  await expect.poll(redPixels).toBe(0);
});
