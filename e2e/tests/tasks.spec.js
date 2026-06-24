// E2E specs for the Tasks / Chores feature (NES-32 / NES-33).
//
// The Playwright config logs in once (auth.setup.js) and reuses storageState,
// so every test here starts ALREADY authenticated as the household owner. Test
// bodies just navigate (`await page.goto('/tasks')`) — never log in.
//
// Note on materialization: task INSTANCES (the rows with Done/Skip/Claim) are
// produced by a ~5-minute background scheduler, so a freshly created recurring
// task does NOT immediately show a row. These specs therefore assert only the
// create redirect + absence of a visible error, never that a new row appears.
const { test, expect } = require('@playwright/test');

// Generates a unique chore title so re-runs never collide on existing data.
function uniqueTitle(prefix) {
  return `${prefix} ${Date.now()}`;
}

test.describe('Tasks / Chores', () => {
  test('GET /tasks renders the chores list (heading, add affordance, rows or empty state)', async ({ page }) => {
    await page.goto('/tasks');

    // The page heading is always present whether or not any instances exist.
    await expect(page.getByRole('heading', { name: 'Chores & Maintenance' })).toBeVisible();

    // The "add" affordance links to the create-task form.
    const addLink = page.getByRole('link', { name: 'Add chore' });
    await expect(addLink).toBeVisible();
    await expect(addLink).toHaveAttribute('href', '/tasks/new');

    // The list renders either real task rows OR the empty state — either is a
    // valid render. We assert the page is in one of those two states rather than
    // depending on background-materialized instances existing.
    const emptyState = page.getByText("No upcoming or overdue tasks — you're all caught up!");
    const anyRow = page.locator('[id^="task-"]');
    const hasEmpty = await emptyState.isVisible().catch(() => false);
    if (!hasEmpty) {
      // Not empty: at least one task row must be present.
      await expect(anyRow.first()).toBeVisible();
    } else {
      await expect(emptyState).toBeVisible();
    }
  });

  test('GET /tasks/new renders the full cadence builder', async ({ page }) => {
    await page.goto('/tasks/new');

    await expect(page.getByRole('heading', { name: 'Add a chore or maintenance task' })).toBeVisible();

    // Core cadence-builder fields are all present.
    await expect(page.locator('input[name="title"]')).toBeVisible();
    await expect(page.locator('input[name="category"][value="chore"]')).toBeAttached();
    await expect(page.locator('input[name="category"][value="maintenance"]')).toBeAttached();
    await expect(page.locator('select[name="freq"]')).toBeVisible();
    await expect(page.locator('input[name="interval"]')).toBeVisible();
    await expect(page.locator('input[name="anchor"]')).toBeVisible();
    await expect(page.locator('select[name="rotation_policy"]')).toBeVisible();
    await expect(page.locator('input[name="points"]')).toBeVisible();
    await expect(page.locator('input[name="lead_time_days"]')).toBeVisible();

    // Save button is present.
    await expect(page.getByRole('button', { name: 'Save chore' })).toBeVisible();
  });

  test('selecting freq=weekly reveals the weekday checkboxes (Alpine x-show)', async ({ page }) => {
    await page.goto('/tasks/new');

    // Default freq is "daily" — the weekday checkboxes are hidden (x-show + x-cloak).
    const sunday = page.locator('input[name="byweekday"][value="0"]');
    await expect(sunday).toBeHidden();

    // Switching to weekly reveals the seven weekday checkboxes.
    await page.locator('select[name="freq"]').selectOption('weekly');
    await expect(sunday).toBeVisible();

    // All seven weekday checkboxes (0..6) become visible.
    for (let day = 0; day <= 6; day++) {
      await expect(page.locator(`input[name="byweekday"][value="${day}"]`)).toBeVisible();
    }

    // Switching back to daily hides them again.
    await page.locator('select[name="freq"]').selectOption('daily');
    await expect(sunday).toBeHidden();
  });

  test('creating a valid claimable chore redirects to /tasks with no visible error', async ({ page }) => {
    await page.goto('/tasks/new');

    await page.locator('input[name="title"]').fill(uniqueTitle('Take out recycling'));
    await page.locator('select[name="freq"]').selectOption('daily');
    await page.locator('input[name="interval"]').fill('1');
    await page.locator('input[name="points"]').fill('5');
    // Claimable hides the rotation pool, so no pool members are required.
    await page.locator('select[name="rotation_policy"]').selectOption('claimable');

    await page.getByRole('button', { name: 'Save chore' }).click();

    // On success the handler issues a 303 redirect to /tasks.
    await page.waitForURL((u) => new URL(u).pathname === '/tasks', { timeout: 10_000 });
    await expect(page.getByRole('heading', { name: 'Chores & Maintenance' })).toBeVisible();

    // No validation error must be visible after a successful create. (We do NOT
    // assert a new row appears — instances are materialized by a background
    // scheduler and will not show immediately.)
    await expect(page.getByRole('alert')).toHaveCount(0);
  });

  test('invalid cadence (interval 0) is rejected with a visible error, not a silent success', async ({ page }) => {
    await page.goto('/tasks/new');

    await page.locator('input[name="title"]').fill(uniqueTitle('Bad cadence chore'));
    await page.locator('select[name="freq"]').selectOption('daily');
    // interval 0 is invalid — the handler requires interval >= 1. The form has
    // novalidate, so the browser will not block this submission; the server must.
    await page.locator('input[name="interval"]').fill('0');
    await page.locator('input[name="points"]').fill('1');
    await page.locator('select[name="rotation_policy"]').selectOption('claimable');

    const createResponse = page.waitForResponse(
      (resp) => resp.url().endsWith('/tasks') && resp.request().method() === 'POST',
    );
    await page.getByRole('button', { name: 'Save chore' }).click();

    // The form must re-render at HTTP 422 — NOT redirect to /tasks.
    const resp = await createResponse;
    expect(resp.status()).toBe(422);

    // A visible error message must be shown. The form posts to action="/tasks";
    // a non-redirect 422 re-renders the form in place, so the browser URL is the
    // action (/tasks), not /tasks/new. The sticky form inputs prove re-render.
    await expect(page.getByRole('alert')).toBeVisible();
    await expect(page.getByRole('alert')).toContainText('Interval must be a whole number of 1 or more.');
    expect(new URL(page.url()).pathname).toBe('/tasks');
    await expect(page.locator('input[name="title"]')).toBeVisible();
  });
});
