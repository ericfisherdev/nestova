// Shared helpers for the NES-171 checklist run.
//
// Personas log in per-test rather than sharing a storageState file. Two
// reasons: most of the checklist's tests need a SPECIFIC persona (child must be
// refused what owner is allowed), and a saved storageState proved unreliable
// here — the captured cookie was not the token the server had persisted, so
// reused contexts arrived unauthenticated.
const { expect } = require('@playwright/test');

// login signs persona in through the real form and leaves the page on the
// dashboard. Throws if the app does not land on "/", so a broken login fails
// the calling test at the point of setup rather than somewhere downstream.
async function login(page, persona) {
  await page.goto('/login');
  await page.fill('input[name="email"]', persona.email);
  await page.fill('input[name="password"]', persona.password);
  await page.click('button:has-text("Sign in")');
  await page.waitForURL((u) => new URL(u).pathname === '/', { timeout: 15_000 });
  await expect(page.locator('h1', { hasText: 'Dashboard' })).toBeVisible();
}

// csrfToken pulls the token out of whatever form is on the current page, for
// the tests that must POST directly (forged-token and role-bypass checks
// cannot go through the UI, which would not offer the control at all).
async function csrfToken(page, path) {
  await page.goto(path);
  return page.locator('input[name="csrf_token"]').first().inputValue();
}

// postForm issues a form POST from inside the browser session and returns the
// HTTP status. Used by the tests that assert a status code (403/422) rather
// than a rendered outcome.
//
// redirect:'manual' is deliberate — following a 303 would hide whether the
// write was accepted. The browser reports a same-origin manual redirect as an
// opaque response with status 0, so that case is mapped back to 303: for these
// tests "redirected" and "accepted" are the same answer.
async function postForm(page, path, fields) {
  return page.evaluate(async ({ path, fields }) => {
    const body = new URLSearchParams(fields).toString();
    const res = await fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body,
      redirect: 'manual',
    });
    return res.type === 'opaqueredirect' ? 303 : res.status;
  }, { path, fields });
}

module.exports = { login, csrfToken, postForm };
