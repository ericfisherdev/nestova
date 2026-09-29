// Idempotent persona provisioning for the checklist run.
//
// Every other spec depends on this one, so it must be safe to run against a
// database that already has the personas. It asserts the end state rather than
// the creation path — the creation path itself is tested in 00-fresh-db.spec.js
// against an empty database.
const { test, expect } = require('@playwright/test');
const { PERSONAS, HOUSEHOLD_A } = require('../tests/fixtures');
const { login } = require('./helpers');
const { psql } = require('../tests/db');

test('personas exist', async ({ page }) => {
  // Household A specifically, not "exactly one household": the §0.2
  // tenant-isolation spec seeds a second household, and this guard must not
  // start failing once it has run.
  const householdA = psql(
    `SELECT count(*) FROM identity.household WHERE name = '${HOUSEHOLD_A}';`,
  ).trim();
  expect(householdA, 'run ./reset-db.sh then --project=fresh first').toBe('1');

  await login(page, PERSONAS.owner);

  for (const persona of [PERSONAS.adult, PERSONAS.child]) {
    const existing = psql(
      `SELECT count(*) FROM identity.member WHERE display_name = '${persona.displayName}';`,
    ).trim();
    if (existing !== '0') continue;

    await page.goto('/members/new');
    await page.fill('input[name="display_name"]', persona.displayName);
    await page.selectOption('select[name="role"]', persona.role);
    await page.fill('input[name="email"]', persona.email);
    await page.fill('input[name="password"]', persona.password);
    await page.click('button:has-text("Add member")');
    await expect(page.locator(`text=${persona.displayName}`).first()).toBeVisible({ timeout: 10_000 });
  }

  expect(Number(psql('SELECT count(*) FROM identity.member;').trim())).toBeGreaterThanOrEqual(3);
});
