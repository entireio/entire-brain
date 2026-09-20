const { defineConfig } = require('@playwright/test');

module.exports = defineConfig({
  testDir: './browser-tests',
  workers: 1,
  retries: 0,
  timeout: 60000,
  use: { browserName: 'chromium', viewport: { width: 1280, height: 800 }, trace: 'retain-on-failure' },
});
