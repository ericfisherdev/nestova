// §2.2 recovery codes and disenrolment.
//
// Its own seeded member, separate from 12-mfa.spec.js: that spec deliberately
// drives the account into a lockout, and a locked account cannot exercise a
// recovery code.
const { test, expect } = require('@playwright/test');
const { PERSONAS, PASSWORD } = require('../tests/fixtures');
const { login, postForm } = require('./helpers');
const { psql, seedMemberInA } = require('../tests/db');
const totp = require('./totp');

const USER = { email: 'mfa-recovery@test.local', password: PASSWORD, displayName: 'MFA Recovery' };

test.describe.configure({ mode: 'serial' });

let secret;
let recoveryCodes = [];

test.beforeAll(() => {
  seedMemberInA({
    displayName: USER.displayName, email: USER.email, role: 'adult',
    copyHashFrom: PERSONAS.owner.email,
  });
  psql(`DELETE FROM identity.member_mfa WHERE member_id IN
          (SELECT id FROM identity.member WHERE email = '${USER.email}');`);
});

test('T-2.2.9a enrol and capture recovery codes', async ({ page }) => {
  await login(page, USER);
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
  secret = await page.locator('#mfa-manual-secret').inputValue();

  // Confirming reveals the recovery codes exactly once.
  await page.evaluate(async ({ csrf_token, code }) => {
    const res = await fetch('/settings/mfa/confirm', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ csrf_token, code }).toString(),
    });
    document.body.innerHTML = await res.text();
  }, { csrf_token, code: totp.code(secret) });

  recoveryCodes = await page.locator('li').allInnerTexts();
  recoveryCodes = recoveryCodes.map((c) => c.trim()).filter((c) => /^[A-Za-z0-9-]{6,}$/.test(c));
  expect(recoveryCodes.length, 'confirmation must reveal recovery codes').toBeGreaterThan(0);
});

test('T-2.2.9 a recovery code satisfies the second factor exactly once', async ({ page }) => {
  const codeToUse = recoveryCodes[0];
  expect(codeToUse, 'the previous test must have captured codes').toBeTruthy();

  async function loginToSecondFactor() {
    await page.goto('/login');
    await page.fill('input[name="email"]', USER.email);
    await page.fill('input[name="password"]', USER.password);
    await page.click('button:has-text("Sign in")');
    await page.waitForURL((u) => new URL(u).pathname.startsWith('/login/mfa'), { timeout: 15_000 });
    return page.locator('input[name="csrf_token"]').first().inputValue();
  }

  let csrf_token = await loginToSecondFactor();
  const first = await postForm(page, '/login/mfa', { csrf_token, recovery_code: codeToUse });
  expect(first, 'a valid recovery code must be accepted').toBe(303);

  csrf_token = await loginToSecondFactor();
  const replay = await postForm(page, '/login/mfa', { csrf_token, recovery_code: codeToUse });
  expect(replay, 'a recovery code must not work twice').toBeGreaterThanOrEqual(400);
});

test('T-2.2.11 disenrolling with a wrong code is refused', async ({ page }) => {
  await page.goto('/login');
  await page.fill('input[name="email"]', USER.email);
  await page.fill('input[name="password"]', USER.password);
  await page.click('button:has-text("Sign in")');
  await page.waitForURL((u) => new URL(u).pathname.startsWith('/login/mfa'), { timeout: 15_000 });
  let csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
  expect(await postForm(page, '/login/mfa', { csrf_token, code: totp.code(secret) })).toBe(303);

  await page.goto('/settings');
  csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
  const status = await postForm(page, '/settings/mfa/disenroll', { csrf_token, totp_code: '000000' });
  expect(status, 'disenrolling with a wrong code must be refused').toBe(401);

  const stillEnrolled = psql(`
    SELECT count(*) FROM identity.member_mfa m
      JOIN identity.member mem ON mem.id = m.member_id
     WHERE mem.email = '${USER.email}';`).trim();
  expect(stillEnrolled, 'a refused disenrolment must leave MFA in place').toBe('1');
});
