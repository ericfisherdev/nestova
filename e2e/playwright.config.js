// Headed Playwright config for the Nestova app.
// Single worker, not parallel: the suite drives one running server and one
// visible browser on the local display.
const { defineConfig, devices } = require('@playwright/test');

module.exports = defineConfig({
  testDir: './tests',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [['list']],
  timeout: 30_000,
  expect: { timeout: 7_000 },
  use: {
    baseURL: process.env.NESTOVA_BASE_URL || 'http://localhost:8099',
    headless: false,
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
    video: 'off',
    launchOptions: { slowMo: 120 },
  },
  projects: [
    { name: 'setup', testMatch: /auth\.setup\.js/ },
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'], headless: false, storageState: '.auth/user.json' },
      dependencies: ['setup'],
      testIgnore: /first-run-setup\.spec\.js/,
    },
    // The first-run wizard runs against an UNCONFIGURED server, so it has no
    // login to reuse: no storageState and no dependency on the auth setup,
    // which would fail before the database exists. Opt in with
    // NESTOVA_E2E_SETUP=1 (see tests/first-run-setup.spec.js).
    {
      name: 'setup-wizard',
      testMatch: /first-run-setup\.spec\.js/,
      use: { ...devices['Desktop Chrome'], headless: false },
    },
  ],
});
