const { test, expect } = require('@playwright/test');

// E2E coverage for the Groceries feature (NES-45): usage tracker, pantry, and
// shopping list. The Playwright config logs in once and ships a storageState, so
// every test starts ALREADY authenticated as the household owner — the bodies
// just navigate to /groceries and never touch the login flow.
//
// Mutations are HTMX POSTs. The server answers a successful mutation with an
// `HX-Redirect: /groceries` header, so htmx performs a client-side redirect back
// to /groceries: the visible state refreshes while the URL stays /groceries.
// Each assertion therefore waits on the synchronously-rendered text/row that the
// refreshed page shows. Restock predictions are produced by a background
// scheduler and are intentionally NOT asserted on here.

// uniqueName returns a collision-free item name so repeated runs (and the three
// sections sharing a page) never match each other's rows.
function uniqueName(prefix) {
  return `${prefix} ${Date.now()}`;
}

// selectFirstUnit picks the first available measurement unit in a unit <select>.
// The option values are server-defined; selecting by index keeps the test from
// hard-coding a unit value that the domain might rename.
async function selectFirstUnit(select) {
  const firstValue = await select.locator('option').first().getAttribute('value');
  await select.selectOption(firstValue);
}

test.describe('Groceries', () => {
  test.beforeEach(async ({ page }) => {
    await page.goto('/groceries');
  });

  test('renders the usage tracker, pantry, and shopping list sections', async ({ page }) => {
    await expect(page).toHaveURL(/\/groceries$/);
    await expect(page.getByRole('heading', { name: 'Groceries', level: 1 })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Usage tracker', level: 2 })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Pantry', level: 2 })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Shopping list', level: 2 })).toBeVisible();

    // The shopping list is grouped by lifecycle status.
    await expect(page.getByRole('heading', { name: 'Needed', level: 3 })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'In cart', level: 3 })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Purchased', level: 3 })).toBeVisible();
  });

  test('registering a tracked item adds it to the usage tracker', async ({ page }) => {
    const itemName = uniqueName('Coffee beans');

    const tracker = page
      .locator('section')
      .filter({ has: page.getByRole('heading', { name: 'Usage tracker' }) });

    await tracker.locator('#register-item-name').fill(itemName);
    await tracker.locator('#register-item-category').fill('pantry');
    await tracker.getByRole('button', { name: 'Register item' }).click();

    // HX-Redirect refreshes /groceries; the URL stays put and the new item shows.
    await expect(page).toHaveURL(/\/groceries$/);
    await expect(tracker.getByText(itemName, { exact: true })).toBeVisible();
  });

  test('adding a pantry item with quantity and unit shows it in the pantry list', async ({ page }) => {
    const itemName = uniqueName('Olive oil');

    const pantry = page
      .locator('section')
      .filter({ has: page.getByRole('heading', { name: 'Pantry' }) });

    await pantry.locator('#pantry-add-name').fill(itemName);
    await pantry.locator('#pantry-add-amount').fill('2');
    await selectFirstUnit(pantry.locator('#pantry-add-unit'));
    await pantry.getByRole('button', { name: 'Add to pantry' }).click();

    await expect(page).toHaveURL(/\/groceries$/);
    // Pantry items render the normalized (lowercased) ingredient canonical name
    // (NES-38 ingredient normalization), so match case-insensitively rather than
    // on the exact-cased input.
    await expect(pantry.getByText(new RegExp(itemName, 'i'))).toBeVisible();
  });

  test('adding an ad-hoc shopping item shows it with a source badge', async ({ page }) => {
    const itemName = uniqueName('Paper towels');

    const shopping = page
      .locator('section')
      .filter({ has: page.getByRole('heading', { name: 'Shopping list' }) });

    await shopping.locator('#shopping-add-name').fill(itemName);
    await shopping.locator('#shopping-add-amount').fill('1');
    await selectFirstUnit(shopping.locator('#shopping-add-unit'));
    await shopping.getByRole('button', { name: 'Add to list' }).click();

    await expect(page).toHaveURL(/\/groceries$/);

    // The new item lands in the "Needed" group with a "Manual" source badge.
    const row = shopping.locator('li').filter({ hasText: itemName });
    await expect(row).toBeVisible();
    await expect(row.getByText('Manual', { exact: true })).toBeVisible();
  });

  test('toggling status moves a shopping item through needed → in-cart → purchased', async ({ page }) => {
    const itemName = uniqueName('Dish soap');

    const shopping = page
      .locator('section')
      .filter({ has: page.getByRole('heading', { name: 'Shopping list' }) });

    // Seed an ad-hoc item; it starts in the "Needed" group.
    await shopping.locator('#shopping-add-name').fill(itemName);
    await shopping.locator('#shopping-add-amount').fill('1');
    await selectFirstUnit(shopping.locator('#shopping-add-unit'));
    await shopping.getByRole('button', { name: 'Add to list' }).click();
    await expect(page).toHaveURL(/\/groceries$/);

    // Each status group is the nearest enclosing <div> of its <h3> heading.
    // `xpath=ancestor::div[1]` selects that wrapper deterministically so the
    // cross-group "moved out of" assertions below scope to a single group.
    const groupForHeading = (label) =>
      shopping
        .getByRole('heading', { name: label, level: 3 })
        .locator('xpath=ancestor::div[1]');
    const neededGroup = groupForHeading('Needed');
    const inCartGroup = groupForHeading('In cart');
    const purchasedGroup = groupForHeading('Purchased');

    // Initially needed: only the row in the Needed group matches.
    await expect(neededGroup.locator('li').filter({ hasText: itemName })).toBeVisible();

    // needed → in-cart. A row exposes buttons for the statuses it is NOT in, so a
    // needed row offers "In cart".
    const neededRow = shopping.locator('li').filter({ hasText: itemName });
    await neededRow.getByRole('button', { name: 'In cart' }).click();
    await expect(page).toHaveURL(/\/groceries$/);
    await expect(inCartGroup.locator('li').filter({ hasText: itemName })).toBeVisible();
    await expect(neededGroup.locator('li').filter({ hasText: itemName })).toHaveCount(0);

    // in-cart → purchased.
    const inCartRow = shopping.locator('li').filter({ hasText: itemName });
    await inCartRow.getByRole('button', { name: 'Purchased' }).click();
    await expect(page).toHaveURL(/\/groceries$/);
    await expect(purchasedGroup.locator('li').filter({ hasText: itemName })).toBeVisible();
    await expect(inCartGroup.locator('li').filter({ hasText: itemName })).toHaveCount(0);
  });
});
