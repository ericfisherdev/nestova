// §0.3 Role escalation — a child must be refused every parent action, and a
// non-owner adult must be refused the two owner-only ones.
//
// Each route is driven by DIRECT POST as well as by GET, because hiding a
// control in the UI is not authorization: the checklist's T-0.3.4 is precisely
// the case where the button is absent but the endpoint still answers.
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, postForm } = require('./helpers');

// Parent-gated GET pages: a child must get 403, not a rendered page.
const PARENT_PAGES = ['/admin/rewards', '/admin/rewards/new', '/trades/history'];

// Parent-gated POSTs. Tokens are minted from a page the child CAN load, so a
// refusal is an authorization decision and not a CSRF rejection.
const PARENT_POSTS = [
  { path: '/admin/rewards', fields: { name: 'Child probe', cost_points: '5' } },
  { path: '/settings/kiosk/generate', fields: { name: 'Child kiosk' } },
];

test.describe('§0.3 role escalation', () => {
  test('T-0.3.1 child is refused parent pages', async ({ page }) => {
    await login(page, PERSONAS.child);
    const failures = [];
    for (const path of PARENT_PAGES) {
      const res = await page.goto(path);
      if (res.status() !== 403) failures.push(`GET ${path} -> ${res.status()}`);
    }
    expect(failures, 'parent-only pages a child could load').toEqual([]);
  });

  test('T-0.3.4 child is refused parent POSTs made directly', async ({ page }) => {
    await login(page, PERSONAS.child);
    // /settings is open to every role, so it is a valid source of a token.
    await page.goto('/settings');
    const token = await page.locator('input[name="csrf_token"]').first().inputValue();

    const failures = [];
    for (const { path, fields } of PARENT_POSTS) {
      const status = await postForm(page, path, { ...fields, csrf_token: token });
      if (status !== 403) failures.push(`POST ${path} -> ${status}`);
    }
    expect(failures, 'parent-only POSTs a child could perform').toEqual([]);
  });

  test('T-0.3.2 non-owner adult is refused MFA reset', async ({ page }) => {
    await login(page, PERSONAS.adult);
    await page.goto('/settings');
    const token = await page.locator('input[name="csrf_token"]').first().inputValue();
    const status = await postForm(page, '/settings/mfa/reset', {
      csrf_token: token, member_id: '00000000-0000-0000-0000-000000000000', password: PERSONAS.adult.password,
    });
    expect(status, 'MFA reset is owner-only').toBe(403);
  });

  test('T-0.3.3 non-owner adult is refused quiet hours', async ({ page }) => {
    await login(page, PERSONAS.adult);
    await page.goto('/settings');
    const token = await page.locator('input[name="csrf_token"]').first().inputValue();
    const status = await postForm(page, '/settings/notify/quiet-hours', {
      csrf_token: token, start: '22:00', end: '07:00',
    });
    expect(status, 'quiet hours are owner-only').toBe(403);
  });

  test('T-0.3.1b parent CAN reach the pages a child cannot', async ({ page }) => {
    // The mirror of T-0.3.1: proves the 403s above are about role, not a
    // route that is simply broken for everyone.
    await login(page, PERSONAS.owner);
    const failures = [];
    for (const path of PARENT_PAGES) {
      const res = await page.goto(path);
      if (res.status() !== 200) failures.push(`GET ${path} -> ${res.status()}`);
    }
    expect(failures, 'parent pages the owner could not load').toEqual([]);
  });
});
