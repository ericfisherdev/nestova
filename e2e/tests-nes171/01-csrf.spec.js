// §0.1 CSRF — every state-changing POST must reject a missing, malformed, or
// foreign token.
//
// These POST directly rather than through the UI on purpose: the UI always
// supplies a valid token, so driving the form can never exercise the failure
// path this section is about.
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, postForm } = require('./helpers');

// A representative state-changing POST from each bounded context. Kept to
// routes that need no pre-existing {id}, so a CSRF failure is unambiguous
// rather than tangled with a not-found.
const ROUTES = [
  { path: '/tasks', fields: { title: 'CSRF probe', cadence_unit: 'daily', interval: '1' } },
  { path: '/members', fields: { display_name: 'CSRF probe', role: 'child', email: 'csrf@test.local', password: 'testtest1234' } },
  { path: '/albums', fields: { name: 'CSRF probe' } },
  { path: '/groceries/items', fields: { name: 'CSRF probe' } },
  { path: '/groceries/pantry', fields: { name: 'CSRF probe', quantity: '1' } },
  { path: '/groceries/shopping', fields: { name: 'CSRF probe' } },
  { path: '/meals/recipes', fields: { name: 'CSRF probe' } },
  { path: '/subscriptions', fields: { name: 'CSRF probe', cost: '1.00', cycle: 'monthly' } },
  { path: '/settings/pin', fields: { pin: '1234' } },
  { path: '/settings/notify/phone', fields: { phone: '+15555550100' } },
  { path: '/admin/rewards', fields: { name: 'CSRF probe', cost_points: '5' } },
];

test.describe('§0.1 CSRF', () => {
  test.beforeEach(async ({ page }) => {
    await login(page, PERSONAS.owner);
  });

  test('T-0.1.1 POST with a missing CSRF token is refused', async ({ page }) => {
    const failures = [];
    for (const { path, fields } of ROUTES) {
      const status = await postForm(page, path, fields);
      if (status !== 403) failures.push(`${path} -> ${status}`);
    }
    expect(failures, 'routes that accepted a POST with no CSRF token').toEqual([]);
  });

  test('T-0.1.2 POST with a malformed CSRF token is refused', async ({ page }) => {
    const failures = [];
    for (const { path, fields } of ROUTES) {
      const status = await postForm(page, path, { ...fields, csrf_token: 'not-a-real-token' });
      if (status !== 403) failures.push(`${path} -> ${status}`);
    }
    expect(failures, 'routes that accepted a malformed CSRF token').toEqual([]);
  });

  test('T-0.1.3 a token minted for another session is refused', async ({ page, browser }) => {
    // Mint a valid token in a SEPARATE session, then replay it in ours.
    const other = await browser.newContext();
    const otherPage = await other.newPage();
    await login(otherPage, PERSONAS.adult);
    await otherPage.goto('/tasks/new');
    const foreign = await otherPage.locator('input[name="csrf_token"]').first().inputValue();
    await other.close();

    const status = await postForm(page, '/tasks', {
      title: 'Foreign token probe', cadence_unit: 'daily', interval: '1', csrf_token: foreign,
    });
    expect(status, 'a foreign session token must not be accepted').toBe(403);
  });

  test('T-0.1.6 the CSRF refusal is a readable page, not a raw dump', async ({ page }) => {
    const body = await page.evaluate(async () => {
      const res = await fetch('/tasks', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: 'title=probe&cadence_unit=daily&interval=1',
        redirect: 'manual',
      });
      return res.text();
    });
    expect(body, 'refusal body should not leak a Go stack trace').not.toMatch(/goroutine|panic:|\.go:\d+/);
  });
});
