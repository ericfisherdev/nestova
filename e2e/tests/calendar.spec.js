// NES-69/70 Calendar e2e specs.
//
// Every test starts ALREADY LOGGED IN as the owner via the storageState wired up
// in playwright.config.js — never log in here. The "Connect Google account"
// button is an hx-post form; the server answers with an HX-Redirect toward
// Google's consent screen. We assert the redirect target is Google WITHOUT
// completing real OAuth — the navigation is intercepted/aborted before it loads
// accounts.google.com.
const { test, expect } = require('@playwright/test');

test.describe('NES-69/70 Calendar', () => {
  test('renders the unified calendar with a connect affordance', async ({ page }) => {
    await page.goto('/calendar');

    await expect(page.getByRole('heading', { name: 'Calendar', level: 1 })).toBeVisible();
    // The unified grid groups events, chores, and subscription renewals.
    await expect(page.getByRole('heading', { name: 'Upcoming' })).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Connected accounts' })).toBeVisible();

    // The prominent "Connect Google account" affordance must be present.
    await expect(page.getByRole('button', { name: 'Connect Google account' })).toBeVisible();
  });

  test('clicking Connect Google account starts OAuth toward accounts.google.com', async ({ page }) => {
    await page.goto('/calendar');

    // Intercept the OAuth navigation so the test never reaches Google's real
    // consent screen. HTMX performs the HX-Redirect as a top-level navigation;
    // aborting any request whose URL host is google captures the target without
    // completing the flow.
    let googleTarget = null;
    await page.route('**/*', (route) => {
      const url = route.request().url();
      if (/(^|\.)google\.com/.test(new URL(url).host)) {
        googleTarget = url;
        return route.abort();
      }
      return route.continue();
    });

    await page.getByRole('button', { name: 'Connect Google account' }).click();

    // Wait until the intercepted Google navigation has been observed. Poll the
    // captured target rather than waiting on a load that we deliberately abort.
    await expect.poll(() => googleTarget, { timeout: 10_000 }).not.toBeNull();
    expect(new URL(googleTarget).host).toContain('google.com');
    // The Google authorization-code endpoint path (google.Endpoint).
    expect(new URL(googleTarget).pathname).toContain('/o/oauth2/');
  });

  test('calendar items, when present, carry a kind indicator', async ({ page }) => {
    await page.goto('/calendar');

    // The grid may be sparse: the current month can legitimately hold no events,
    // chores, or renewals. Assert against whatever is synchronously present —
    // either the empty-state copy or a list of kind-labelled items.
    const items = page.getByTestId('calendar-items');
    const itemCount = await items.locator('li').count();

    if (itemCount === 0) {
      await expect(page.getByText('Nothing scheduled in this range.')).toBeVisible();
      test.info().annotations.push({
        type: 'note',
        description: 'Calendar grid was empty this run; kind-indicator assertion skipped (sparse data is expected).',
      });
      return;
    }

    // Each rendered item leads with an uppercase kind label: Event, Chore, or
    // Renewal. The first cell of the first row must match one of those.
    const firstKind = (await items.locator('li').first().locator('span').first().textContent())?.trim() ?? '';
    expect(firstKind).toMatch(/^(Event|Chore|Renewal)$/i);
  });
});
