// E2E coverage for the first-run setup wizard and administrator onboarding
// (NES-164 regression).
//
// This spec is the ONE flow that cannot run against the configured app the rest
// of the suite drives: it needs a server started with no persisted state file
// and an EMPTY database, so it runs in its own Playwright project
// ("setup-wizard") with no storageState and no auth.setup dependency, and only
// when NESTOVA_E2E_SETUP=1 opts in.
//
// Start the server under test like this, against a database with no nestova
// schema yet:
//
//   PORT=8099 APP_ENV=dev NESTOVA_FORCE_SETUP=1 \
//     NESTOVA_STATE_FILE=/tmp/nestova-e2e/state.json ./bin/server
//
// then run:
//
//   NESTOVA_E2E_SETUP=1 NESTOVA_SETUP_DB_PORT=5442 \
//     NESTOVA_EMAIL=... NESTOVA_PASSWORD=... \
//     npx playwright test --project=setup-wizard
//
// Completing the wizard makes the server migrate the database, persist its
// state file, and restart itself in normal mode on the same port.
const { test, expect } = require('@playwright/test');
const { requireEnv } = require('./env');

const enabled = process.env.NESTOVA_E2E_SETUP === '1';

test.describe('First-run setup', () => {
  test.skip(!enabled, 'set NESTOVA_E2E_SETUP=1 and point the suite at an unconfigured server');

  // The wizard connects, runs every migration, then restarts the app; the
  // default per-test timeout is far too short for that.
  test.setTimeout(180_000);

  test('the wizard configures the database and the owner account can be created', async ({ page }) => {
    await page.goto('/setup');

    // The form must actually render: a zero RequestTimeout in setup mode used to
    // serve an empty 200 here (NES-164), so assert a field, not just the URL.
    await expect(page.getByRole('heading', { name: 'Welcome to Nestova' })).toBeVisible();
    await expect(page.locator('select[name="provider"]')).toBeVisible();

    await page.locator('select[name="provider"]').selectOption('postgres');
    await page.locator('input[name="host"]').fill(process.env.NESTOVA_SETUP_DB_HOST || 'localhost');
    await page.locator('input[name="port"]').fill(process.env.NESTOVA_SETUP_DB_PORT || '5432');
    await page.locator('input[name="database"]').fill(process.env.NESTOVA_SETUP_DB_NAME || 'nest');
    await page.locator('input[name="user"]').fill(process.env.NESTOVA_SETUP_DB_USER || 'nestova');
    await page.locator('input[name="password"]').fill(process.env.NESTOVA_SETUP_DB_PASSWORD || 'nestova');
    await page.locator('select[name="sslmode"]').selectOption('disable');

    // Guard against a mistyped field before submitting: a wrong value here comes
    // back as a generic connection error that is tedious to tell apart from a
    // genuine failure.
    await expect(page.locator('input[name="port"]')).toHaveValue(
      process.env.NESTOVA_SETUP_DB_PORT || '5432',
    );

    await page.getByRole('button', { name: 'Connect & continue' }).click();

    // Connecting must succeed against an EMPTY database: the wizard's own
    // migrations are what create the required schema (NES-164).
    await expect(page.getByRole('heading', { name: "You're all set" })).toBeVisible({ timeout: 120_000 });
    await expect(page.getByRole('alert')).toHaveCount(0);

    // The completion page refreshes to /onboarding once the app has restarted.
    await page.waitForURL((u) => new URL(u).pathname === '/onboarding', { timeout: 120_000 });
    await expect(page.locator('input[name="household_name"]')).toBeVisible({ timeout: 60_000 });

    await page.locator('input[name="household_name"]').fill('E2E Household');
    await page.locator('input[name="display_name"]').fill('E2E Owner');
    await page.locator('input[name="email"]').fill(requireEnv('NESTOVA_EMAIL'));
    await page.locator('input[name="password"]').fill(requireEnv('NESTOVA_PASSWORD'));
    await page.locator('form button[type="submit"]').click();

    // Onboarding signs the new owner in and lands on the dashboard.
    await page.waitForURL((u) => new URL(u).pathname === '/', { timeout: 30_000 });
    await expect(page).toHaveTitle(/Nestova/);
    await expect(page.getByRole('heading', { name: 'Family' })).toBeVisible();
  });
});
