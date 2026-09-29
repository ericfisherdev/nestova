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
const {
  seedFreshMember, enrollTOTP, passwordStep, postForReveal, revealedRecoveryCodes,
} = require('./helpers-webauthn');

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

// ---------------------------------------------------------------------------
// The tests below each seed their OWN member, independent of the serial USER
// state above.
// ---------------------------------------------------------------------------

// mfaRowCount reports how many MFA enrolments memberId has (0 or 1).
function mfaRowCount(memberId) {
  return psql(`SELECT count(*) FROM identity.member_mfa WHERE member_id = '${memberId}';`).trim();
}

test('T-2.2.10 regenerating recovery codes invalidates every old code', async ({ page }) => {
  const user = seedFreshMember({ email: 'mfa-regen@test.local', displayName: 'MFA Regen' });
  await login(page, user);
  const { secret, recoveryCodes: oldCodes } = await enrollTOTP(page);

  await page.goto('/settings');
  const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
  const { status, html } = await postForReveal(page, '/settings/mfa/recovery-codes/regenerate', { csrf_token, code: totp.code(secret) });
  expect(status, 'regenerating with a valid TOTP code must succeed').toBe(200);
  const newCodes = revealedRecoveryCodes(html);
  expect(newCodes.length, 'regeneration must reveal a new batch').toBe(oldCodes.length);
  expect(newCodes.filter((c) => oldCodes.includes(c)), 'no old code may be reissued').toEqual([]);

  // Only the new batch is stored: every old row is gone, not merely unused.
  const stored = psql(`SELECT count(*) FROM identity.member_recovery_code WHERE member_id = '${user.id}';`).trim();
  expect(stored, 'only the regenerated codes may remain').toBe(String(newCodes.length));

  // Three old codes over HTTP (under the five-failure lockout), then a new
  // one as the sanity guard that the route accepts a valid recovery code.
  const loginToken = await passwordStep(page, user);
  for (const old of oldCodes.slice(0, 3)) {
    expect(await postForm(page, '/login/mfa', { csrf_token: loginToken, recovery_code: old }), `old code ${old} must be refused`).toBe(401);
  }
  expect(await postForm(page, '/login/mfa', { csrf_token: loginToken, recovery_code: newCodes[0] }), 'a new code must work').toBe(303);
});

test('T-2.2.12 only the owner, with their password, can reset another member\'s MFA', async ({ page }) => {
  const target = seedFreshMember({ email: 'mfa-reset-target@test.local', displayName: 'MFA Reset Target' });
  // A dedicated second owner, so the shared owner persona is never involved.
  const owner = seedFreshMember({ email: 'mfa-reset-owner@test.local', displayName: 'MFA Reset Owner', role: 'owner' });
  const adult = seedFreshMember({ email: 'mfa-reset-adult@test.local', displayName: 'MFA Reset Adult', role: 'adult' });

  await login(page, target);
  await enrollTOTP(page);
  expect(mfaRowCount(target.id)).toBe('1');

  async function resetAs(actor, ownerPassword) {
    await page.context().clearCookies();
    await login(page, actor);
    await page.goto('/settings');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
    return postForm(page, '/settings/mfa/reset', { csrf_token, member_id: target.id, owner_password: ownerPassword });
  }

  expect(await resetAs(adult, PASSWORD), 'an adult must be refused').toBe(403);
  expect(mfaRowCount(target.id), 'a refused reset must leave MFA in place').toBe('1');

  expect(await resetAs(owner, 'not-the-password'), 'a wrong owner password must be refused').toBe(401);
  expect(mfaRowCount(target.id), 'a refused reset must leave MFA in place').toBe('1');

  expect(await resetAs(owner, PASSWORD), 'the owner with the right password must succeed').toBe(303);
  expect(mfaRowCount(target.id), 'the reset must remove the enrolment').toBe('0');

  // The target now signs in with the password alone.
  await page.context().clearCookies();
  await login(page, target);
});
