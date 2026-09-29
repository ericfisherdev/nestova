// §4 Dashboard — zero automated coverage before this run.
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login } = require('./helpers');

const CARDS = ['Calendar', 'Chores', 'Meals & Recipes', 'Groceries', 'Photos', 'Subscriptions'];
const NAV = ['Calendar', 'Chores', 'Rewards', 'Meals & Recipes', 'Groceries', 'Subscriptions', 'Photos'];

test.describe('§4 dashboard', () => {
  test('T-4.1.1 all six summary cards render', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const missing = [];
    for (const card of CARDS) {
      if (await page.locator(`text=${card}`).count() === 0) missing.push(card);
    }
    expect(missing, 'dashboard cards that did not render').toEqual([]);
  });

  test('T-4.1.2 the sidebar offers all seven destinations', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const missing = [];
    for (const label of NAV) {
      if (await page.locator('nav a', { hasText: label }).count() === 0) missing.push(label);
    }
    expect(missing, 'sidebar destinations that were absent').toEqual([]);
  });

  test('T-4.1.3 the active nav pill is marked for assistive tech', async ({ page }) => {
    await login(page, PERSONAS.owner);
    for (const [path, label] of [['/tasks', 'Chores'], ['/photos', 'Photos'], ['/calendar', 'Calendar']]) {
      await page.goto(path);
      const active = page.locator(`nav a[aria-current="page"]`);
      await expect(active, `${path} should mark its own pill current`).toHaveCount(1);
      await expect(active).toContainText(label);
    }
  });

  test('T-4.2.3 a child can load the dashboard', async ({ page }) => {
    await login(page, PERSONAS.child);
    await expect(page.locator('h1', { hasText: 'Dashboard' })).toBeVisible();
  });
});
