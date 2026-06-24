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
    },
  ],
});
