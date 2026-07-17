// Auth setup: logs in once and saves the storage state for the other specs.
const { test, expect } = require('@playwright/test');
const fs = require('fs');
const { requireEnv } = require('./env');

const AUTH_FILE = '.auth/user.json';

test('authenticate', async ({ page }) => {
  await page.goto('/login');
  await page.fill('input[name="email"]', requireEnv('NESTOVA_EMAIL'));
  await page.fill('input[name="password"]', requireEnv('NESTOVA_PASSWORD'));
  await page.click('button:has-text("Sign in")');

  // Successful login redirects to the dashboard ("/").
  await page.waitForURL((u) => new URL(u).pathname === '/', { timeout: 15_000 });
  await expect(page).toHaveTitle(/Nestova/);

  fs.mkdirSync('.auth', { recursive: true });
  await page.context().storageState({ path: AUTH_FILE });
});
