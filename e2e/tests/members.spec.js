// E2E specs for the Members feature (add household member).
//
// The Playwright config logs in once (auth.setup.js) and reuses storageState,
// so every test here starts ALREADY authenticated as the household owner. Test
// bodies just navigate (`await page.goto('/members/new')`) — never log in.
const { test, expect } = require('@playwright/test');

// Generates a display name unique to this run so the sidebar assertion can not
// match a member left over from a previous run (display names are not unique,
// so we make ours unique on purpose).
function uniqueName(prefix) {
  return `${prefix} ${Date.now()}`;
}

test.describe('Members', () => {
  test('GET /members/new renders the add-member form', async ({ page }) => {
    await page.goto('/members/new');

    await expect(page.getByRole('heading', { name: 'Add a household member' })).toBeVisible();

    // Required + optional fields are all present.
    await expect(page.locator('input[name="display_name"]')).toBeVisible();
    await expect(page.locator('select[name="role"]')).toBeVisible();
    await expect(page.locator('input[name="email"]')).toBeVisible();
    await expect(page.locator('input[name="password"]')).toBeVisible();

    await expect(page.getByRole('button', { name: 'Add member' })).toBeVisible();
  });

  test('creating a member with a unique name succeeds and appears in the dashboard Family sidebar', async ({ page }) => {
    const name = uniqueName('Member');

    await page.goto('/members/new');
    await page.locator('input[name="display_name"]').fill(name);
    await page.locator('select[name="role"]').selectOption('child');
    // Email + password are optional; leaving both blank creates a non-login member.

    await page.getByRole('button', { name: 'Add member' }).click();

    // On success the handler issues a 303 redirect to the dashboard ("/").
    await page.waitForURL((u) => new URL(u).pathname === '/', { timeout: 10_000 });

    // No validation error must be visible after a successful create.
    await expect(page.getByRole('alert')).toHaveCount(0);

    // The new member appears in the sidebar Family section. Scope to the section
    // whose "Family" heading anchors the member list so we don't match the name
    // elsewhere on the page.
    await page.goto('/');
    const familyHeading = page.getByRole('heading', { name: 'Family' });
    await expect(familyHeading).toBeVisible();
    // The Family heading + member rows share a wrapping <div> in the sidebar
    // (not a <section>); scope to it so the name match can't be satisfied by
    // unrelated page text.
    const familySection = familyHeading.locator('xpath=ancestor::div[1]');
    await expect(familySection.getByText(name, { exact: true })).toBeVisible();
  });

  test('submitting with an empty display name is rejected with a visible error', async ({ page }) => {
    await page.goto('/members/new');

    // display_name is required server-side ("Display name is required."). The
    // form carries novalidate, so the browser will not block the empty submit;
    // the server must reject it at HTTP 422.
    await page.locator('input[name="display_name"]').fill('');
    await page.locator('select[name="role"]').selectOption('adult');

    const createResponse = page.waitForResponse(
      (resp) => resp.url().endsWith('/members') && resp.request().method() === 'POST',
    );
    await page.getByRole('button', { name: 'Add member' }).click();

    const resp = await createResponse;
    expect(resp.status()).toBe(422);

    // A visible error message must be shown, and we must stay on the form.
    await expect(page.getByRole('alert')).toBeVisible();
    await expect(page.getByRole('alert')).toContainText('Display name is required.');
    expect(new URL(page.url()).pathname).toBe('/members');
  });
});
