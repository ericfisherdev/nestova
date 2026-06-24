const { test, expect } = require('@playwright/test');

// Edge case: auth + CSRF enforcement. Protected routes require a member; HTMX
// requests get 401, full navigations redirect to /login; mutating POSTs require
// a CSRF token; onboarding is closed once a household exists.

const BASE = process.env.NESTOVA_BASE_URL || 'http://localhost:8099';

// anonContext makes a guaranteed-anonymous context. Passing an explicit empty
// storageState overrides the project's use.storageState, which the test runner
// otherwise applies to browser.newContext().
async function anonContext(browser) {
  return browser.newContext({ baseURL: BASE, storageState: { cookies: [], origins: [] } });
}

test('unauthenticated full navigation to a protected route redirects to /login', async ({ browser }) => {
  const ctx = await anonContext(browser);
  try {
    const page = await ctx.newPage();
    await page.goto('/tasks', { waitUntil: 'domcontentloaded' });
    await expect(page).toHaveURL(/\/login/);
  } finally {
    await ctx.close();
  }
});

test('unauthenticated HTMX request to a protected route is 401', async ({ browser }) => {
  const ctx = await anonContext(browser);
  try {
    const resp = await ctx.request.get('/groceries', {
      headers: { 'HX-Request': 'true' },
      maxRedirects: 0,
    });
    expect(resp.status()).toBe(401);
  } finally {
    await ctx.close();
  }
});

test('a mutating POST without a CSRF token is rejected (403)', async ({ page }) => {
  // page carries the authenticated session (storageState); omit csrf_token.
  const resp = await page.request.post('/subscriptions', {
    form: { name: 'NoCSRF', amount: '9.99', currency: 'USD', cycle: 'monthly', next_renewal_on: '2030-01-01' },
    maxRedirects: 0,
  });
  expect(resp.status()).toBe(403);
});

test('onboarding is closed once a household exists (redirects to /login)', async ({ page }) => {
  // Precondition (made explicit): a household exists — the authenticated session
  // resolves to a member, so the dashboard renders at "/" rather than bouncing to
  // /login (unauthenticated) or /onboarding (no household).
  await page.goto('/', { waitUntil: 'domcontentloaded' });
  await expect(page).toHaveURL(/\/$/);

  await page.goto('/onboarding', { waitUntil: 'domcontentloaded' });
  await expect(page).toHaveURL(/\/login/);
});
