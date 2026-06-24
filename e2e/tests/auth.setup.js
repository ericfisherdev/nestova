// Auth setup: logs in once and saves the storage state for the other specs.
const { test, expect } = require('@playwright/test');
const fs = require('fs');

const AUTH_FILE = '.auth/user.json';

test('authenticate', async ({ page }) => {
  await page.goto('/login');
  await page.fill('input[name="email"]', process.env.NESTOVA_EMAIL || 'eric@ericfisher.dev');
  await page.fill('input[name="password"]', process.env.NESTOVA_PASSWORD || 'testtest');
  await page.click('button:has-text("Sign in")');

  // Successful login redirects to the dashboard ("/").
  await page.waitForURL((u) => new URL(u).pathname === '/', { timeout: 15_000 });
  await expect(page).toHaveTitle(/Nestova/);

  fs.mkdirSync('.auth', { recursive: true });
  await page.context().storageState({ path: AUTH_FILE });
});
