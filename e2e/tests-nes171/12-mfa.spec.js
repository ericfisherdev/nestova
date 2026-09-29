// §2.2 TOTP multi-factor — security-critical and previously uncovered.
//
// Runs against its OWN seeded member: enrolling MFA changes every later login
// for that account, so using a shared persona would break the rest of the suite.
const { test, expect } = require('@playwright/test');
const { PERSONAS, PASSWORD } = require('../tests/fixtures');
const { login, postForm } = require('./helpers');
const { psql, seedMemberInA } = require('../tests/db');
const totp = require('./totp');

const MFA_USER = { email: 'mfa-probe@test.local', password: PASSWORD, displayName: 'MFA Probe' };

test.describe.configure({ mode: 'serial' });

test.beforeAll(() => {
  seedMemberInA({
    displayName: MFA_USER.displayName,
    email: MFA_USER.email,
    role: 'adult',
    copyHashFrom: PERSONAS.owner.email,
  });
  // Start from a known state so a re-run is not affected by a previous one.
  psql(`DELETE FROM identity.member_mfa WHERE member_id IN
          (SELECT id FROM identity.member WHERE email = '${MFA_USER.email}');`);
});

// enroll POSTs the enrolment request and returns the revealed base32 secret.
async function enroll(page) {
  await page.goto('/settings');
  const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
  await page.evaluate(async ({ csrf_token }) => {
    const res = await fetch('/settings/mfa/enroll', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ csrf_token }).toString(),
    });
    document.body.innerHTML = await res.text();
  }, { csrf_token });

  const secret = await page.locator('#mfa-manual-secret').inputValue();
  expect(secret, 'enrolment must reveal a manual-entry secret').toBeTruthy();
  return { secret, csrf_token };
}

test('T-2.2.1 enrolment reveals a scannable secret', async ({ page }) => {
  await login(page, MFA_USER);
  const { secret } = await enroll(page);
  expect(secret.replace(/\s/g, ''), 'the secret should be base32').toMatch(/^[A-Z2-7]+=*$/);
});

test('T-2.2.3 confirming with a wrong code is refused', async ({ page }) => {
  await login(page, MFA_USER);
  const { csrf_token } = await enroll(page);
  const status = await postForm(page, '/settings/mfa/confirm', { csrf_token, code: '000000' });
  expect(status, 'a wrong confirmation code must be refused').toBe(401);
});

test('T-2.2.6 malformed confirmation codes are refused', async ({ page }) => {
  await login(page, MFA_USER);
  const { csrf_token } = await enroll(page);
  const accepted = [];
  for (const code of ['12345', '1234567', 'abcdef', '']) {
    const status = await postForm(page, '/settings/mfa/confirm', { csrf_token, code });
    if (status < 400) accepted.push(`${JSON.stringify(code)} -> ${status}`);
  }
  expect(accepted, 'malformed codes that were accepted').toEqual([]);
});

test('T-2.2.4 an expired code from a previous window is refused', async ({ page }) => {
  await login(page, MFA_USER);
  const { secret, csrf_token } = await enroll(page);
  // Five periods ago — well outside any tolerated skew.
  const stale = totp.code(secret, { stepOffset: -5 });
  const status = await postForm(page, '/settings/mfa/confirm', { csrf_token, code: stale });
  expect(status, 'a long-expired code must be refused').toBe(401);
});

test('T-2.2.2 confirming with a valid code enrols the member', async ({ page }) => {
  await login(page, MFA_USER);
  const { secret, csrf_token } = await enroll(page);
  const status = await postForm(page, '/settings/mfa/confirm', { csrf_token, code: totp.code(secret) });
  expect(status, 'a valid confirmation code must be accepted').toBe(200);

  // Keep the secret for the login tests below.
  process.env.__MFA_SECRET = secret;

  const confirmed = psql(`
    SELECT count(*) FROM identity.member_mfa m
      JOIN identity.member mem ON mem.id = m.member_id
     WHERE mem.email = '${MFA_USER.email}';`).trim();
  expect(confirmed, 'an MFA row must exist after confirmation').toBe('1');
});

test('T-2.2.7 login now demands a second factor', async ({ page }) => {
  await page.goto('/login');
  await page.fill('input[name="email"]', MFA_USER.email);
  await page.fill('input[name="password"]', MFA_USER.password);
  await page.click('button:has-text("Sign in")');

  await page.waitForURL((u) => new URL(u).pathname.startsWith('/login/mfa'), { timeout: 15_000 });
  expect(new URL(page.url()).pathname, 'an enrolled member must be sent to the MFA step').toBe('/login/mfa');
});

test('T-2.2.8 the app is unreachable between the two login steps', async ({ page }) => {
  await page.goto('/login');
  await page.fill('input[name="email"]', MFA_USER.email);
  await page.fill('input[name="password"]', MFA_USER.password);
  await page.click('button:has-text("Sign in")');
  await page.waitForURL((u) => new URL(u).pathname.startsWith('/login/mfa'), { timeout: 15_000 });

  const res = await page.request.get('/settings', { maxRedirects: 0 });
  expect(res.status(), 'a half-authenticated session must not reach the app').not.toBe(200);
});

test('T-2.2.4b completing the second factor signs the member in', async ({ page }) => {
  const secret = process.env.__MFA_SECRET;
  expect(secret, 'the enrolment test must have run first').toBeTruthy();

  await page.goto('/login');
  await page.fill('input[name="email"]', MFA_USER.email);
  await page.fill('input[name="password"]', MFA_USER.password);
  await page.click('button:has-text("Sign in")');
  await page.waitForURL((u) => new URL(u).pathname.startsWith('/login/mfa'), { timeout: 15_000 });

  const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
  const status = await postForm(page, '/login/mfa', { csrf_token, code: totp.code(secret) });
  expect(status, 'a correct second factor must be accepted').toBe(303);
});

test('T-2.2.13 repeated wrong codes lock the second factor', async ({ page }) => {
  // Its OWN member. The lockout lives in memory for pinBackoffWindow minutes
  // and is not cleared between runs, so driving the shared MFA persona into it
  // would fail the NEXT run of this file — which is exactly what happened
  // before this was split out.
  const lockUser = { email: 'mfa-lockout@test.local', password: MFA_USER.password, displayName: 'MFA Lockout' };
  seedMemberInA({
    displayName: lockUser.displayName, email: lockUser.email, role: 'adult',
    copyHashFrom: PERSONAS.owner.email,
  });
  psql(`DELETE FROM identity.member_mfa WHERE member_id IN
          (SELECT id FROM identity.member WHERE email = '${lockUser.email}');`);

  await login(page, lockUser);
  const { secret, csrf_token: enrolToken } = await enroll(page);
  expect(await postForm(page, '/settings/mfa/confirm', { csrf_token: enrolToken, code: totp.code(secret) })).toBe(200);

  await page.goto('/login');
  await page.fill('input[name="email"]', lockUser.email);
  await page.fill('input[name="password"]', lockUser.password);
  await page.click('button:has-text("Sign in")');
  await page.waitForURL((u) => new URL(u).pathname.startsWith('/login/mfa'), { timeout: 15_000 });
  const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();

  const statuses = [];
  for (let i = 0; i < 8; i += 1) {
    statuses.push(await postForm(page, '/login/mfa', { csrf_token, code: '000000' }));
  }
  expect(statuses.every((s) => s >= 400), 'every wrong code must be refused').toBe(true);

  // After the threshold, even a CORRECT code must be refused while locked.
  const correct = await postForm(page, '/login/mfa', { csrf_token, code: totp.code(secret) });
  expect(correct, 'a correct code during lockout must still be refused').toBeGreaterThanOrEqual(400);
});
