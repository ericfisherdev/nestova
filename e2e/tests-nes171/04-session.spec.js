// §0.4 session and auth state, plus the two CSRF cases that depend on a
// session ending (T-0.1.4, T-0.1.5).
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, postForm } = require('./helpers');

const PROTECTED = ['/', '/tasks', '/settings', '/photos', '/calendar', '/groceries', '/meals', '/subscriptions', '/rewards'];

test.describe('§0.4 session and auth state', () => {
  test('T-0.4.1 protected routes redirect to login when signed out', async ({ page }) => {
    const failures = [];
    for (const path of PROTECTED) {
      const res = await page.goto(path);
      const url = new URL(page.url());
      if (res.status() !== 200 || url.pathname !== '/login') {
        failures.push(`${path} -> ${res.status()} at ${url.pathname}`);
      }
    }
    expect(failures, 'protected routes that did not land on /login while signed out').toEqual([]);
  });

  test('T-0.4.2 a tampered session cookie signs the user out rather than erroring', async ({ page, context }) => {
    await login(page, PERSONAS.owner);
    const cookies = await context.cookies();
    const session = cookies.find((c) => c.name === 'session');
    expect(session, 'a session cookie should exist after login').toBeTruthy();

    await context.clearCookies();
    await context.addCookies([{ ...session, value: `${session.value.slice(0, -4)}AAAA` }]);

    const res = await page.goto('/settings');
    expect(res.status(), 'a tampered cookie must not produce a 5xx').toBeLessThan(500);
    expect(new URL(page.url()).pathname, 'a tampered cookie must land on /login').toBe('/login');
  });

  test('T-0.4.3 the session cookie changes on login (fixation)', async ({ page, context }) => {
    await page.goto('/login');
    const before = (await context.cookies()).find((c) => c.name === 'session');

    await page.fill('input[name="email"]', PERSONAS.owner.email);
    await page.fill('input[name="password"]', PERSONAS.owner.password);
    await page.click('button:has-text("Sign in")');
    await page.waitForURL((u) => new URL(u).pathname === '/', { timeout: 15_000 });

    const after = (await context.cookies()).find((c) => c.name === 'session');
    expect(after, 'a session cookie must exist after login').toBeTruthy();
    if (before) {
      expect(after.value, 'the pre-login token must not survive login').not.toBe(before.value);
    }
  });

  test('T-0.4.4 logout invalidates the session server-side', async ({ page }) => {
    await login(page, PERSONAS.owner);
    await page.click('form[action="/logout"] button');
    await page.waitForURL((u) => new URL(u).pathname === '/login', { timeout: 15_000 });

    // Not just "the UI moved on": the old cookie must no longer authenticate.
    const res = await page.goto('/settings');
    expect(new URL(page.url()).pathname, 'a logged-out session must not reach /settings').toBe('/login');
    expect(res.status()).toBeLessThan(500);
  });

  test('T-0.1.4/T-0.1.5 a token from an ended session is refused', async ({ page }) => {
    await login(page, PERSONAS.owner);
    await page.goto('/tasks/new');
    const stale = await page.locator('input[name="csrf_token"]').first().inputValue();

    await page.goto('/');
    await page.click('form[action="/logout"] button');
    await page.waitForURL((u) => new URL(u).pathname === '/login', { timeout: 15_000 });

    // Sign back in, then replay the token minted before the logout.
    await login(page, PERSONAS.owner);
    const status = await postForm(page, '/tasks', {
      csrf_token: stale, title: 'Stale token probe', category: 'chore', freq: 'daily',
      interval: '1', rotation_policy: 'claimable', photo_policy: 'none', points: '5', lead_time_days: '0',
    });
    expect(status, 'a token minted before logout must not be accepted after re-login').toBe(403);
  });

  test('T-0.4.5 two concurrent sessions are independent', async ({ browser }) => {
    const a = await browser.newContext();
    const b = await browser.newContext();
    const pageA = await a.newPage();
    const pageB = await b.newPage();
    await login(pageA, PERSONAS.owner);
    await login(pageB, PERSONAS.owner);

    await pageA.click('form[action="/logout"] button');
    await pageA.waitForURL((u) => new URL(u).pathname === '/login', { timeout: 15_000 });

    // B must be unaffected by A signing out.
    const res = await pageB.goto('/settings');
    expect(res.status(), 'the second session must survive the first signing out').toBe(200);
    expect(new URL(pageB.url()).pathname).toBe('/settings');

    await a.close();
    await b.close();
  });
});
