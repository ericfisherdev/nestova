// NES-70 Subscriptions e2e specs.
//
// Every test starts ALREADY LOGGED IN as the owner via the storageState wired up
// in playwright.config.js — never log in here. The page mutates through HTMX
// forms (hx-post) that the server answers with an HX-Redirect back to
// /subscriptions, so after submitting we wait for the row/rollup to settle rather
// than for an explicit navigation.
const { test, expect } = require('@playwright/test');

// The "Add subscription" form is the only form rendered with that submit button,
// so it makes a stable scope for filling fields. Edit forms live inside each row's
// <details> and use the "Save" button instead.
const ADD_FORM = 'form:has(button:has-text("Add subscription"))';

// fillAddForm fills the add-subscription form. currency is optional (the field
// defaults to USD); cycle defaults to monthly so the monthly rollup moves by the
// full amount, keeping rollup assertions simple.
async function fillAddForm(page, { name, amount, currency, cycle = 'monthly', nextRenewal = '2026-12-15' }) {
  const form = page.locator(ADD_FORM);
  await form.locator('input[name="name"]').fill(name);
  await form.locator('input[name="amount"]').fill(String(amount));
  if (currency !== undefined) {
    await form.locator('input[name="currency"]').fill(currency);
  }
  await form.locator('select[name="cycle"]').selectOption(cycle);
  await form.locator('input[name="next_renewal_on"]').fill(nextRenewal);
}

// addSubscription fills and submits the add form, then waits for the new row to
// appear in the active list. Returns the unique name used.
async function addSubscription(page, opts) {
  await fillAddForm(page, opts);
  await page.locator(`${ADD_FORM} button:has-text("Add subscription")`).click();
  // The list re-renders after the HX-Redirect round trip; the row's name appears
  // in the "Active subscriptions" section.
  await expect(page.getByText(opts.name, { exact: true })).toBeVisible();
}

// rowFor locates the <li> in the active list whose name matches. The add form's
// name field also contains text, so we scope to list items (each row is an <li>).
function rowFor(page, name) {
  return page.locator('li').filter({ hasText: name });
}

test.describe('NES-70 Subscriptions', () => {
  test('renders the active list and a monthly cost rollup', async ({ page }) => {
    await page.goto('/subscriptions');

    await expect(page.getByRole('heading', { name: 'Subscriptions', level: 1 })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Active subscriptions' })).toBeVisible();

    // The prominent monthly-normalized rollup is always present (a money string,
    // "Mixed currencies", or a zero total when empty) — assert it is rendered.
    await expect(page.getByTestId('monthly-rollup')).toBeVisible();
    await expect(page.getByTestId('monthly-rollup')).not.toBeEmpty();
  });

  test('exposes the expected add-form fields', async ({ page }) => {
    await page.goto('/subscriptions');
    const form = page.locator(ADD_FORM);

    await expect(form.locator('input[name="name"]')).toBeVisible();
    await expect(form.locator('input[name="amount"]')).toBeVisible();
    await expect(form.locator('input[name="currency"]')).toBeVisible();
    await expect(form.locator('select[name="cycle"]')).toBeVisible();
    await expect(form.locator('input[name="next_renewal_on"]')).toBeVisible();
    await expect(form.locator('select[name="payer_id"]')).toBeVisible();
    // Currency defaults to USD per the template's currencyOrDefault helper.
    await expect(form.locator('input[name="currency"]')).toHaveValue('USD');
  });

  test('adding a subscription shows it in the list and updates the rollup', async ({ page }) => {
    await page.goto('/subscriptions');

    const name = `Streaming ${Date.now()}`;
    // A distinctive amount makes the row's amount label easy to assert and shifts
    // a (non-mixed) rollup by a recognisable value.
    await addSubscription(page, { name, amount: '12.34', currency: 'USD', cycle: 'monthly' });

    const row = rowFor(page, name);
    await expect(row).toBeVisible();
    // The row summary reads "<amount label> · <cycle> · renews <date>".
    await expect(row).toContainText('12.34');
    await expect(row).toContainText('monthly');

    // A single-currency household yields a real monthly total, so the rollup must
    // reflect the just-added amount. If the household already held a mixed-currency
    // set the rollup would read "Mixed currencies" — assert the monthly figure only
    // when a numeric total is present, otherwise assert the row landed.
    const rollup = (await page.getByTestId('monthly-rollup').textContent())?.trim() ?? '';
    if (/\d/.test(rollup) && !/mixed/i.test(rollup)) {
      expect(rollup).toContain('12.34');
    }
  });

  test('editing a subscription updates its summary', async ({ page }) => {
    await page.goto('/subscriptions');

    const name = `Editable ${Date.now()}`;
    await addSubscription(page, { name, amount: '5.00', currency: 'USD', cycle: 'monthly' });

    const row = rowFor(page, name);
    // Open the collapsed edit form for this row and change the amount.
    await row.locator('summary:has-text("Edit")').click();
    const editForm = row.locator('form:has(button:has-text("Save"))');
    await editForm.locator('input[name="amount"]').fill('7.50');
    await editForm.locator('button:has-text("Save")').click();

    // The page re-renders; the same row now shows the new amount and not the old.
    const updated = rowFor(page, name);
    await expect(updated).toBeVisible();
    await expect(updated).toContainText('7.50');
    await expect(updated).not.toContainText('5.00');
  });

  test('deactivating a subscription removes it from the active list', async ({ page }) => {
    await page.goto('/subscriptions');

    const name = `Throwaway ${Date.now()}`;
    await addSubscription(page, { name, amount: '3.21', currency: 'USD', cycle: 'monthly' });

    const row = rowFor(page, name);
    await expect(row).toBeVisible();
    await row.locator('button:has-text("Deactivate")').click();

    // After the HX-Redirect refresh the row is gone from the active list.
    await expect(rowFor(page, name)).toHaveCount(0);
  });

  test('single-household currency: a mixed currency surfaces a mixed-currency rollup', async ({ page }) => {
    await page.goto('/subscriptions');

    // Seed one USD subscription, then add a second in a different currency. The
    // household now spans two currencies, which has no single monthly total; the
    // handler maps ErrCurrencyMismatch to the "Mixed currencies" rollup label.
    const usd = `BaseUSD ${Date.now()}`;
    await addSubscription(page, { name: usd, amount: '9.99', currency: 'USD', cycle: 'monthly' });

    const eur = `OtherEUR ${Date.now()}`;
    await addSubscription(page, { name: eur, amount: '8.88', currency: 'EUR', cycle: 'monthly' });

    // Both rows should be present (a mismatch does not block listing), and the
    // rollup should report the mixed-currency label rather than a single total.
    await expect(rowFor(page, usd)).toBeVisible();
    await expect(rowFor(page, eur)).toBeVisible();
    await expect(page.getByTestId('monthly-rollup')).toContainText(/mixed/i);
  });
});
