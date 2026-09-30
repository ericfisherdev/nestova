// §0.4 session and auth state, plus the two CSRF cases that depend on a
// session ending (T-0.1.4, T-0.1.5).
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, csrfToken, postForm } = require('./helpers');
const { psql, seedMemberInA } = require('../tests/db');

const TS = Date.now();

// postNoFollow posts through the page's cookie jar and reports the raw status
// and Location. postForm maps every redirect to 303, which cannot tell "saved,
// back to the page" from "signed out, go to /login" — this case needs both.
async function postNoFollow(page, path, fields) {
  const res = await page.request.post(path, { form: fields, maxRedirects: 0 });
  return { status: res.status(), location: res.headers().location || '' };
}

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
    expect(before, 'GET /login must issue a pre-login session (it holds the CSRF token)').toBeTruthy();
    expect(after.value, 'the pre-login token must not survive login').not.toBe(before.value);
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

  test('T-0.4.6 a deactivated or deleted member loses their live session', async ({ browser }) => {
    const seed = (label) => {
      const email = `${label}-${TS}@test.local`;
      const id = seedMemberInA({ displayName: `${label} ${TS}`, email, role: 'adult', copyHashFrom: PERSONAS.owner.email });
      return { id, email, password: PERSONAS.owner.password, label };
    };
    const members = [seed('deactivated'), seed('deleted')];

    const sessions = [];
    for (const m of members) {
      const context = await browser.newContext();
      const page = await context.newPage();
      await login(page, m);
      // Sanity guard: the live session works and its write is accepted.
      const csrf_token = await csrfToken(page, '/settings');
      const before = await postNoFollow(page, '/admin/rewards', { csrf_token, name: `${m.label} before ${TS}`, cost_points: '5' });
      expect(before.status, `${m.label}: the live session's write is accepted`).toBe(303);
      expect(before.location, `${m.label}: an accepted write does not bounce to /login`).not.toContain('/login');
      sessions.push({ m, context, page, csrf_token });
    }

    psql(`UPDATE identity.member SET active = false, updated_at = now() WHERE id = '${members[0].id}';`);
    psql(`DELETE FROM identity.member WHERE id = '${members[1].id}';`);

    for (const { m, context, page, csrf_token } of sessions) {
      const after = await postNoFollow(page, '/admin/rewards', { csrf_token, name: `${m.label} after ${TS}`, cost_points: '5' });
      expect(after.status, `${m.label}: the stale session's write must not be accepted`).toBe(303);
      expect(after.location, `${m.label}: the stale session is sent to sign in`).toContain('/login');
      expect(psql(`SELECT count(*) FROM nestova.reward WHERE name = '${m.label} after ${TS}';`).trim(),
        `${m.label}: nothing may be written by the stale session`).toBe('0');

      const res = await page.goto('/settings');
      expect(res.status()).toBeLessThan(500);
      expect(new URL(page.url()).pathname, `${m.label}: a page load lands on /login`).toBe('/login');
      await context.close();
    }

    // And the deactivated member cannot simply sign back in.
    const context = await browser.newContext();
    const page = await context.newPage();
    await page.goto('/login');
    await page.fill('input[name="email"]', members[0].email);
    await page.fill('input[name="password"]', members[0].password);
    await page.click('button:has-text("Sign in")');
    await page.waitForLoadState('load');
    expect(new URL(page.url()).pathname, 'a deactivated member must not sign in again').toBe('/login');
    await context.close();
  });
});
