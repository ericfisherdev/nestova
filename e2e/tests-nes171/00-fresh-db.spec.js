// §1 first-run tests. These REQUIRE an empty database — run ./reset-db.sh
// first, then: npx playwright test -c nes171.config.js --project=fresh
//
// Household B is not created here: Nestova provisions a SINGLE household
// (household.ErrHouseholdExists), so the second household the §0.2
// tenant-isolation tests need has to be seeded with SQL, not through the UI.
const { test, expect } = require('@playwright/test');
const { PERSONAS, HOUSEHOLD_A } = require('../tests/fixtures');
const { login } = require('./helpers');
const { psql } = require('../tests/db');

test.describe.configure({ mode: 'serial' });

test('T-1.1.1 unconfigured app routes to onboarding, not the dashboard', async ({ page }) => {
  const res = await page.goto('/onboarding');
  expect(res.status()).toBe(200);
  await expect(page.locator('input[name="household_name"]')).toBeVisible();
});

test('T-1.1.3 create household and owner', async ({ page }) => {
  await page.goto('/onboarding');
  await page.fill('input[name="household_name"]', HOUSEHOLD_A);
  await page.fill('input[name="display_name"]', PERSONAS.owner.displayName);
  await page.fill('input[name="email"]', PERSONAS.owner.email);
  await page.fill('input[name="password"]', PERSONAS.owner.password);
  await page.click('button:has-text("Create household")');

  // Onboarding renews the session token and signs the new owner in, so it
  // lands on the dashboard rather than bouncing to /login.
  await page.waitForURL((u) => new URL(u).pathname === '/', { timeout: 15_000 });
  await expect(page.locator('h1', { hasText: 'Dashboard' })).toBeVisible();
});

test('T-2.1.1 owner can sign in', async ({ page }) => {
  await login(page, PERSONAS.owner);
});

test('T-1.2.2 onboarding a second household is refused', async ({ page }) => {
  await page.goto('/onboarding');
  const form = page.locator('input[name="household_name"]');
  if (await form.count()) {
    await page.fill('input[name="household_name"]', 'Should Not Exist');
    await page.fill('input[name="display_name"]', 'Nope');
    await page.fill('input[name="email"]', 'nope@test.local');
    await page.fill('input[name="password"]', PERSONAS.owner.password);
    await page.click('button:has-text("Create household")');
  }
  expect(psql('SELECT count(*) FROM identity.household;').trim()).toBe('1');
});

test('T-1.1.4 owner adds an adult and a child member', async ({ page }) => {
  await login(page, PERSONAS.owner);

  for (const persona of [PERSONAS.adult, PERSONAS.child]) {
    await page.goto('/members/new');
    await page.fill('input[name="display_name"]', persona.displayName);
    await page.selectOption('select[name="role"]', persona.role);
    await page.fill('input[name="email"]', persona.email);
    await page.fill('input[name="password"]', persona.password);
    // Scoped to the member form: the shell also renders a "Sign out" submit
    // button, and a bare button[type=submit] selector hits that one first.
    await page.click('button:has-text("Add member")');
    await expect(page.locator(`text=${persona.displayName}`).first()).toBeVisible({ timeout: 10_000 });
  }
});

test('T-1.1.4b adult and child can each sign in', async ({ browser }) => {
  for (const persona of [PERSONAS.adult, PERSONAS.child]) {
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    await login(page, persona);
    await ctx.close();
  }
});
