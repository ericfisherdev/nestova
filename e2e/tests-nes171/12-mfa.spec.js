// §2.2 TOTP multi-factor — security-critical and previously uncovered.
//
// Runs against its OWN seeded member: enrolling MFA changes every later login
// for that account, so using a shared persona would break the rest of the suite.
const { test, expect } = require('@playwright/test');
const { PERSONAS, PASSWORD } = require('../tests/fixtures');
const { login, postForm } = require('./helpers');
const { psql, seedMemberInA } = require('../tests/db');
const totp = require('./totp');
const {
  seedFreshMember, enrollTOTP, passwordStep, signInWithPassword, waitForTOTPWindowRoom,
} = require('./helpers-webauthn');

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

// ---------------------------------------------------------------------------
// The tests below each seed their OWN member (seedFreshMember), so they do not
// depend on the serial MFA_USER state above, nor on each other.
// ---------------------------------------------------------------------------

// REMEMBER_COOKIE is the "remember this device" cookie the login MFA step
// sets (authadapter.RememberDeviceCookieName).
const REMEMBER_COOKIE = 'nestova_remember';
const THIRTY_DAYS_S = 30 * 24 * 60 * 60;

// enrolledMember seeds a fresh member, signs in (no second factor yet) and
// enrols TOTP. Returns the persona and its secret.
async function enrolledMember(page, email, displayName) {
  const user = seedFreshMember({ email, displayName });
  await login(page, user);
  const { secret } = await enrollTOTP(page);
  return { user, secret };
}

// rememberedLogin completes the second factor with "remember this device"
// ticked and returns the remember cookie it set.
async function rememberedLogin(page, user, secret) {
  const csrf_token = await passwordStep(page, user);
  const status = await postForm(page, '/login/mfa', { csrf_token, code: totp.code(secret), remember_device: 'on' });
  expect(status, 'a correct code with remember-device must be accepted').toBe(303);
  const cookie = (await page.context().cookies()).find((c) => c.name === REMEMBER_COOKIE);
  expect(cookie, 'remember-device must set its cookie').toBeTruthy();
  return cookie;
}

test('T-2.2.5 a login TOTP code cannot be replayed', async ({ page }) => {
  const { user, secret } = await enrolledMember(page, 'mfa-replay@test.local', 'MFA Replay');

  // Both logins must fall inside ONE period, or the second code is simply a
  // different code and the test proves nothing.
  await waitForTOTPWindowRoom(page, 15);
  const code = totp.code(secret);

  let csrf_token = await passwordStep(page, user);
  expect(await postForm(page, '/login/mfa', { csrf_token, code }), 'the first use must be accepted').toBe(303);

  csrf_token = await passwordStep(page, user);
  expect(await postForm(page, '/login/mfa', { csrf_token, code }), 'the same code a second time must be refused').toBe(401);
});

test('T-2.2.17 clock skew of one period is tolerated, five minutes is not', async ({ page }) => {
  const { user, secret } = await enrolledMember(page, 'mfa-skew@test.local', 'MFA Skew');

  // Enough room that a "-1" code cannot age into "-2" before the server checks it.
  await waitForTOTPWindowRoom(page, 15);
  let csrf_token = await passwordStep(page, user);

  // Ten periods = five minutes, either side. Refusals first: they do not
  // advance the replay high-water mark, and two stay under the lockout.
  for (const stepOffset of [-10, 10]) {
    const status = await postForm(page, '/login/mfa', { csrf_token, code: totp.code(secret, { stepOffset }) });
    expect(status, `a code ${stepOffset} periods off must be refused`).toBe(401);
  }

  const behind = await postForm(page, '/login/mfa', { csrf_token, code: totp.code(secret, { stepOffset: -1 }) });
  expect(behind, 'a code one period behind must be accepted').toBe(303);

  // +1 is a LATER step than -1, so the replay guard does not refuse it.
  csrf_token = await passwordStep(page, user);
  const ahead = await postForm(page, '/login/mfa', { csrf_token, code: totp.code(secret, { stepOffset: 1 }) });
  expect(ahead, 'a code one period ahead must be accepted').toBe(303);
});

test('T-2.2.14 a remembered device skips the prompt, for 30 days', async ({ page }) => {
  const { user, secret } = await enrolledMember(page, 'mfa-remember@test.local', 'MFA Remember');
  const cookie = await rememberedLogin(page, user, secret);

  // The token is opaque; its expiry lives server-side in remembered_device, and
  // the cookie's own lifetime must agree with it. Neither clock can be
  // advanced from here, so the 30-day bound is asserted on both values rather
  // than waited out.
  const nowS = Date.now() / 1000;
  expect(Math.abs(cookie.expires - (nowS + THIRTY_DAYS_S)), 'cookie lifetime must be 30 days').toBeLessThan(300);
  const storedExpiryS = Number(psql(`SELECT extract(epoch FROM expires_at)::bigint FROM nestova.remembered_device
                                       WHERE member_id = '${user.id}';`).trim());
  expect(Math.abs(storedExpiryS - (nowS + THIRTY_DAYS_S)), 'stored expiry must be 30 days out').toBeLessThan(300);
  const storedHash = psql(`SELECT encode(token_hash, 'hex') FROM nestova.remembered_device
                            WHERE member_id = '${user.id}';`).trim();
  expect(storedHash, 'only a 32-byte hash of the token may be stored').toMatch(/^[0-9a-f]{64}$/);
  expect(storedHash, 'the raw token must not be what is stored').not.toBe(Buffer.from(cookie.value, 'base64url').toString('hex'));

  // Sign out but keep the remember cookie: the password alone now lands on
  // the dashboard (helpers.login asserts "/").
  await page.context().clearCookies();
  await page.context().addCookies([cookie]);
  await login(page, user);
});

test('T-2.2.15 a remember cookie copied to another browser stops working once revoked', async ({ page, browser, baseURL }) => {
  // The token is a server-side bearer credential: a copy made BEFORE any
  // revocation is indistinguishable from the original (a user agent is
  // attacker-controlled, so binding to it would add nothing). What the ticket
  // requires is that revocation kills every copy, so revoke through the real
  // path: the household owner resets the member's MFA, and the member enrols
  // again.
  const owner = seedFreshMember({ email: 'mfa-remember-owner@test.local', displayName: 'MFA Remember Owner', role: 'owner' });
  const { user, secret } = await enrolledMember(page, 'mfa-remember-move@test.local', 'MFA Remember Move');
  const cookie = await rememberedLogin(page, user, secret);

  const other = await browser.newContext({ baseURL });
  try {
    const otherPage = await other.newPage();
    // Sanity: without the cookie, the other browser IS prompted.
    await passwordStep(otherPage, user);
    await other.clearCookies();

    // Revoke: owner reset, then the member enrols a fresh secret.
    await page.context().clearCookies();
    await login(page, owner);
    await page.goto('/settings');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
    expect(await postForm(page, '/settings/mfa/reset', { csrf_token, member_id: user.id, owner_password: PASSWORD }),
      'the owner reset must succeed').toBe(303);
    expect(psql(`SELECT count(*) FROM nestova.remembered_device WHERE member_id = '${user.id}';`).trim(),
      'a reset must delete the member\'s remembered devices').toBe('0');
    await page.context().clearCookies();
    await login(page, user);
    await enrollTOTP(page);

    await other.addCookies([cookie]);
    expect(await signInWithPassword(otherPage, user), 'a revoked remember cookie must not skip MFA').toBe('/login/mfa');
  } finally {
    await other.close();
  }
});

test('T-2.2.16 enrolling twice is refused and keeps the original secret', async ({ page }) => {
  const { user, secret } = await enrolledMember(page, 'mfa-twice@test.local', 'MFA Twice');
  const secretRow = () => psql(`SELECT encode(totp_secret_enc, 'hex') FROM identity.member_mfa m
                                  JOIN identity.member mem ON mem.id = m.member_id
                                 WHERE mem.email = '${user.email}';`).trim();
  const before = secretRow();

  await page.goto('/settings');
  const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
  // ErrMFAAlreadyEnrolled surfaces as a plain redirect back to /settings with
  // no secret revealed (MFAWebHandlers.Enroll).
  const res = await page.request.post('/settings/mfa/enroll', { form: { csrf_token }, maxRedirects: 0 });
  expect(res.status(), 'a second enrolment must be turned away').toBe(303);
  expect(res.headers().location).toBe('/settings');
  expect(await res.text()).not.toContain('mfa-manual-secret');
  expect(secretRow(), 'the stored secret must not change').toBe(before);

  // And the ORIGINAL secret still signs the member in.
  const loginToken = await passwordStep(page, user);
  expect(await postForm(page, '/login/mfa', { csrf_token: loginToken, code: totp.code(secret) })).toBe(303);
});
