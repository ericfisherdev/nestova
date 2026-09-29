// §2.1 password login: error wording, enumeration, input handling, logout,
// brute-force limiting and oversized passwords.
//
// Wrong-password traffic is aimed at members seeded for this file, never at
// the shared owner/adult/child: once login rate limiting exists (T-2.1.7), a
// burst of failures against a shared persona would lock the rest of the suite
// out.
const { test, expect } = require('@playwright/test');
const { PERSONAS, PASSWORD } = require('../tests/fixtures');
const { seedMemberInA } = require('../tests/db');
const { login, csrfToken } = require('./helpers');

const GENERIC_ERROR = 'Invalid email or password.';
const PROTECTED = ['/', '/tasks', '/settings', '/photos', '/calendar', '/groceries', '/meals', '/subscriptions', '/rewards', '/members/new'];

const PROBE = { displayName: 'Login Probe', email: 'login-probe@test.local' };
const RATE_PROBE = { displayName: 'Rate Limit Probe', email: 'ratelimit-probe@test.local' };

test.beforeAll(() => {
  for (const probe of [PROBE, RATE_PROBE]) {
    seedMemberInA({ ...probe, role: 'adult', copyHashFrom: PERSONAS.owner.email });
  }
});

// postLogin submits the login form from inside the page (so the session and
// CSRF cookie are the browser's own) and reports status, the alert text, the
// raw body and the round-trip time. A 303 comes back as an opaque redirect.
async function postLogin(page, email, password) {
  const csrf_token = await csrfToken(page, '/login');
  return page.evaluate(async (fields) => {
    const started = performance.now();
    const res = await fetch('/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams(fields).toString(),
      redirect: 'manual',
    });
    const ms = performance.now() - started;
    if (res.type === 'opaqueredirect') return { status: 303, alert: '', body: '', ms };
    const body = await res.text();
    const doc = new DOMParser().parseFromString(body, 'text/html');
    const alert = doc.querySelector('[role="alert"]');
    return { status: res.status, alert: alert ? alert.textContent.trim() : '', body, ms };
  }, { csrf_token, email, password });
}

function median(values) {
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.floor(sorted.length / 2)];
}

test.describe('§2.1 password login', () => {
  test('T-2.1.2 a wrong password gives the generic error with no hint which field was wrong', async ({ page }) => {
    const ok = await postLogin(page, PROBE.email, PASSWORD);
    expect(ok.status, 'sanity: the right password signs in').toBe(303);

    await page.context().clearCookies();
    await page.goto('/login');
    await page.fill('input[name="email"]', PROBE.email);
    await page.fill('input[name="password"]', 'wrong-password-123');
    const [res] = await Promise.all([
      page.waitForResponse((r) => r.url().endsWith('/login') && r.request().method() === 'POST'),
      page.click('button:has-text("Sign in")'),
    ]);
    expect(res.status()).toBe(401);
    await expect(page.getByRole('alert')).toHaveText(GENERIC_ERROR);
    // No per-field hint: nothing flags the password (or email) input itself.
    await expect(page.locator('[aria-invalid="true"]')).toHaveCount(0);
    expect(await page.locator('body').innerText()).not.toMatch(/incorrect password|wrong password|password is|no account|not found|unknown/i);
    await expect(page.locator('input[name="password"]'), 'the password is never echoed back').toHaveValue('');
  });

  test('T-2.1.3 an unknown email gets the identical response, in similar time, as a wrong password', async ({ page }) => {
    // Warm both paths once so first-request costs don't skew the medians.
    await postLogin(page, PROBE.email, 'wrong-password-123');
    await postLogin(page, `nobody-${Date.now()}@test.local`, 'wrong-password-123');

    const wrong = [];
    const unknown = [];
    for (let i = 0; i < 7; i += 1) {
      wrong.push(await postLogin(page, PROBE.email, `wrong-password-${i}`));
      unknown.push(await postLogin(page, `nobody-${i}-${Date.now()}@test.local`, `wrong-password-${i}`));
    }

    for (const r of [...wrong, ...unknown]) {
      expect(r.status).toBe(401);
      expect(r.alert).toBe(GENERIC_ERROR);
    }
    const wrongMs = median(wrong.map((r) => r.ms));
    const unknownMs = median(unknown.map((r) => r.ms));
    const ratio = unknownMs / wrongMs;
    // The unknown-email path runs a dummy argon2id verify, so both paths cost
    // one hash. Without it, the unknown path is a bare DB miss — an order of
    // magnitude faster, far outside this band.
    expect(ratio, `unknown ${unknownMs.toFixed(1)}ms vs wrong ${wrongMs.toFixed(1)}ms`).toBeGreaterThan(0.5);
    expect(ratio, `unknown ${unknownMs.toFixed(1)}ms vs wrong ${wrongMs.toFixed(1)}ms`).toBeLessThan(2);
  });

  test('T-2.1.4 an empty email or empty password is refused cleanly, never a 5xx', async ({ page }) => {
    expect((await postLogin(page, PROBE.email, PASSWORD)).status, 'sanity: a full payload signs in').toBe(303);
    await page.context().clearCookies();

    const cases = {
      'empty email': ['', PASSWORD],
      'empty password': [PROBE.email, ''],
      'both empty': ['', ''],
      'whitespace email': ['   ', PASSWORD],
    };
    for (const [label, [email, password]] of Object.entries(cases)) {
      const r = await postLogin(page, email, password);
      expect(r.status, label).toBe(401);
      expect(r.alert, label).toBe(GENERIC_ERROR);
    }
  });

  test('T-2.1.5 email is case-insensitive and surrounding whitespace is trimmed', async ({ browser }) => {
    const variants = [
      PROBE.email.toUpperCase(),
      'Login-Probe@Test.Local',
      `  ${PROBE.email}  `,
      `\t${PROBE.email.toUpperCase()} `,
    ];
    for (const email of variants) {
      const ctx = await browser.newContext();
      const page = await ctx.newPage();
      const r = await postLogin(page, email, PASSWORD);
      expect(r.status, JSON.stringify(email)).toBe(303);
      await page.goto('/');
      await expect(page.locator('h1', { hasText: 'Dashboard' }), JSON.stringify(email)).toBeVisible();
      await ctx.close();
    }
    // The password, by contrast, is NOT trimmed: a padded password is wrong.
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    expect((await postLogin(page, PROBE.email, ` ${PASSWORD} `)).status).toBe(401);
    await ctx.close();
  });

  test('T-2.1.6 after logout every protected route bounces to /login', async ({ page, browser, context }) => {
    await login(page, PERSONAS.owner);
    const session = (await context.cookies()).find((c) => c.name === 'session');
    await page.click('form[action="/logout"] button');
    await page.waitForURL((u) => new URL(u).pathname === '/login', { timeout: 15_000 });

    const leaked = [];
    for (const path of PROTECTED) {
      const res = await page.goto(path);
      const at = new URL(page.url()).pathname;
      if (at !== '/login' || res.status() >= 400) leaked.push(`${path} -> ${res.status()} at ${at}`);
    }
    expect(leaked, 'protected routes reachable after logout').toEqual([]);

    // The pre-logout cookie, replayed in a fresh browser, is dead too.
    const replay = await browser.newContext();
    await replay.addCookies([session]);
    const replayPage = await replay.newPage();
    await replayPage.goto('/tasks');
    expect(new URL(replayPage.url()).pathname, 'a replayed pre-logout session').toBe('/login');
    await replay.close();
  });

  test('T-2.1.7 repeated failures are rate limited without confirming the account exists', async ({ page }) => {
    test.fail(true, 'DEFECT: password login has no attempt limiting (NES-86 never implemented; only the MFA step is limited) — 20 wrong passwords in a row all return 401');
    expect((await postLogin(page, RATE_PROBE.email, PASSWORD)).status, 'sanity: the right password signs in').toBe(303);
    await page.context().clearCookies();

    const known = [];
    const unknown = [];
    const ghost = `ghost-${Date.now()}@test.local`;
    for (let i = 0; i < 20; i += 1) {
      known.push(await postLogin(page, RATE_PROBE.email, `wrong-password-${i}`));
      unknown.push(await postLogin(page, ghost, `wrong-password-${i}`));
    }
    const throttled = (r) => r.status === 429 || /too many|try again later|locked/i.test(r.alert);

    // The limiter must engage for the real account...
    expect(known.some(throttled), `statuses: ${known.map((r) => r.status).join(',')}`).toBe(true);
    // ...and a burst against a non-existent account must look the same, or
    // the lockout itself becomes an account-existence oracle.
    expect(unknown.map(throttled), 'unknown email must throttle identically').toEqual(known.map(throttled));
    expect(unknown.map((r) => r.alert)).toEqual(known.map((r) => r.alert));
  });

  test('T-2.1.8 [!] a 10,000-character password is rejected promptly, without hanging argon2id', async ({ page }) => {
    const samples = [];
    for (let i = 0; i < 3; i += 1) samples.push((await postLogin(page, PROBE.email, `wrong-password-${i}`)).ms);
    const baseline = median(samples);

    const huge = 'p'.repeat(10_000);
    for (const email of [PROBE.email, `nobody-${Date.now()}@test.local`]) {
      const r = await postLogin(page, email, huge);
      expect(r.status, email).toBe(401);
      expect(r.alert, email).toBe(GENERIC_ERROR);
      expect(r.ms, `${email}: ${r.ms.toFixed(0)}ms vs ${baseline.toFixed(0)}ms baseline`).toBeLessThan(Math.max(2_000, baseline * 5));
    }
    expect((await postLogin(page, PROBE.email, PASSWORD)).status, 'sanity: the real password still signs in').toBe(303);
  });
});
