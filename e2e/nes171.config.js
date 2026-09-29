// Playwright config for the NES-171 checklist run.
//
// Kept separate from playwright.config.js so the existing 16-spec suite is not
// disturbed: this run starts from an EMPTY database and bootstraps its own
// personas, where the existing suite assumes a household already exists.
//
// Headed and single-worker on purpose — the checklist's concurrency tests
// (§0.7) drive deliberate races, and a parallel runner would make their
// outcomes ambiguous.
const { defineConfig, devices } = require('@playwright/test');

module.exports = defineConfig({
  testDir: './tests-nes171',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [['list']],
  timeout: 45_000,
  expect: { timeout: 7_000 },
  use: {
    baseURL: process.env.NESTOVA_BASE_URL || 'http://localhost:8099',
    headless: false,
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
    video: 'off',
    launchOptions: { slowMo: 60 },
  },
  projects: [
    // Bootstrap runs against an empty database with no stored session: it
    // creates household A, its members, and household B.
    {
      name: 'fresh',
      testMatch: /00-fresh-db\.spec\.js/,
      use: { ...devices['Desktop Chrome'], headless: false },
    },
    // Specs log in per-test via helpers.login (see helpers.js) rather than
    // sharing a storageState file.
    {
      name: 'bootstrap',
      testMatch: /00-bootstrap\.spec\.js/,
      use: { ...devices['Desktop Chrome'], headless: false },
    },
    {
      name: 'checklist',
      testIgnore: /00-(bootstrap|fresh-db)\.spec\.js/,
      use: { ...devices['Desktop Chrome'], headless: false },
      dependencies: ['bootstrap'],
    },
  ],
});
