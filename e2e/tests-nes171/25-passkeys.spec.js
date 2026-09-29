// §2.3 Passkeys (WebAuthn), driven through a Chromium CDP virtual
// authenticator.
//
// Every test seeds its OWN member: registering or revoking a passkey changes
// how that account signs in, so the shared personas are never used here.
//
// All but T-2.3.7 need a server with WebAuthn wired, which only happens when
// PUBLIC_BASE_URL is set; see helpers-webauthn.js. T-2.3.7 is the opposite
// case and runs against the ordinary checklist server.
const crypto = require('crypto');
const { test, expect } = require('@playwright/test');
const { psql } = require('../tests/db');
const { login } = require('./helpers');
const totp = require('./totp');
const wa = require('./helpers-webauthn');

const NO_WEBAUTHN_SERVER = 'NESTOVA_WEBAUTHN_BASE_URL is unset: passkeys need a server started with '
  + 'PUBLIC_BASE_URL equal to its own origin (see helpers-webauthn.js)';
const LOGIN_FINISH = '/login/passkey/finish';
const STEP_UP_BEGIN = '/login/mfa/passkey/begin';
const STEP_UP_FINISH = '/login/mfa/passkey/finish';
const OVERSIZED_BODY = JSON.stringify({ id: 'x', type: 'public-key', padding: 'a'.repeat(70 * 1024) });
const SIGN_COUNT_ANOMALY_TITLE = 'Unusual passkey activity detected';

// passkeyMember seeds a password-only member, attaches a virtual
// authenticator to page, signs in and registers one passkey through the real
// settings UI. Returns the persona, the authenticator handle, and the
// credential as the authenticator stores it (private key included).
async function passkeyMember(page, { email, displayName, nickname = 'Test passkey' }) {
  const user = wa.seedFreshMember({ email, displayName });
  const authenticator = await wa.addVirtualAuthenticator(page);
  expect(await wa.signInWithPassword(page, user), 'a password-only member lands on the dashboard').toBe('/');
  await wa.registerPasskeyThroughUI(page, nickname);
  const credential = await credentialOf(authenticator, user);
  return { user, authenticator, credential };
}

// credentialOf picks user's credential out of the authenticator, matched on
// the user handle the server stored at registration.
async function credentialOf(authenticator, user) {
  const handle = psql(`SELECT encode(user_handle, 'base64') FROM identity.member_credential
                        WHERE member_id = '${user.id}' LIMIT 1;`).trim();
  expect(handle, 'the registration must be stored').toBeTruthy();
  const credential = (await wa.storedCredentials(authenticator)).find((c) => c.userHandle === handle);
  expect(credential, 'the authenticator must hold the registered credential').toBeTruthy();
  return credential;
}

// beginLogin signs out, opens /login and starts a usernameless passkey
// ceremony. Returns the CSRF token for finish and the server's challenge.
async function beginLogin(page) {
  await page.context().clearCookies();
  await page.goto('/login');
  const csrf = await wa.passkeyCSRFToken(page);
  const begin = await wa.fetchInPage(page, '/login/passkey/begin');
  expect(begin.status, 'passkey login must start').toBe(200);
  return { csrf, challenge: JSON.parse(begin.body).publicKey.challenge };
}

// beginStepUp puts user on /login/mfa (password accepted, second factor
// pending) and starts the passkey step-up ceremony there.
async function beginStepUp(page, user) {
  const csrf = await wa.passwordStep(page, user);
  const begin = await wa.fetchInPage(page, STEP_UP_BEGIN);
  expect(begin.status, 'passkey step-up must start').toBe(200);
  return { csrf, challenge: JSON.parse(begin.body).publicKey.challenge };
}

// clickPasskeyButton clicks a passkey button and returns the finish response.
async function clickPasskeyButton(page, label, finishPath) {
  const finished = page.waitForResponse((r) => new URL(r.url()).pathname === finishPath);
  await page.click(`button:has-text("${label}")`);
  return finished;
}

// devicesCSRFToken reads the token the "Your devices" section carries for
// the JSON registration endpoints.
async function devicesCSRFToken(page) {
  await page.goto('/settings');
  return page.locator('#webauthn-devices').getAttribute('data-csrf-token');
}

function storedNickname(user) {
  return psql(`SELECT nickname FROM identity.member_credential WHERE member_id = '${user.id}';`).trim();
}

function credentialRowId(user) {
  return psql(`SELECT id FROM identity.member_credential WHERE member_id = '${user.id}';`).trim();
}

function credentialCount(user) {
  return psql(`SELECT count(*) FROM identity.member_credential WHERE member_id = '${user.id}';`).trim();
}

function signCountAnomalies(user) {
  return Number(psql(`SELECT count(*) FROM nestova.notification
                       WHERE member_id = '${user.id}' AND title = '${SIGN_COUNT_ANOMALY_TITLE}';`).trim());
}

// renameCSRFToken reads the token from the device row's rename form.
async function renameCSRFToken(page) {
  await page.goto('/settings');
  return page.locator('#webauthn-devices li input[name="csrf_token"]').first().inputValue();
}

test.describe('unwired server', () => {
  test('T-2.3.7 with WebAuthn unwired the passkey button is hidden and every endpoint is 404', async ({ page, baseURL }) => {
    await page.goto('/login');
    await expect(page.locator('button[type="submit"]:has-text("Sign in")')).toBeVisible();
    await expect(page.getByText('Sign in with passkey')).toHaveCount(0);
    for (const [method, path] of [['GET', '/login/passkey/begin'], ['POST', LOGIN_FINISH]]) {
      const res = await page.request.fetch(path, { method, maxRedirects: 0 });
      expect(res.status(), `${method} ${path} must not exist`).toBe(404);
    }

    const user = wa.seedFreshMember({ email: 'pk-unwired@test.local', displayName: 'PK Unwired' });
    await login(page, user);
    const settingsHTML = await (await page.request.get('/settings')).text();
    expect(settingsHTML, 'the settings page must not offer passkeys').not.toContain('webauthn-devices');
    const csrf = await page.locator('input[name="csrf_token"]').first().inputValue();
    const someId = crypto.randomUUID();
    for (const path of ['/settings/webauthn/register/begin', '/settings/webauthn/register/finish',
      `/settings/webauthn/${someId}/rename`, `/settings/webauthn/${someId}/revoke`]) {
      const res = await page.request.post(path, { headers: { 'X-CSRF-Token': csrf }, form: { csrf_token: csrf }, maxRedirects: 0 });
      expect(res.status(), `POST ${path} must not exist`).toBe(404);
    }

    // The step-up routes stay registered and 404 themselves; reaching them
    // needs a pending second factor, so enrol TOTP first.
    await wa.enrollTOTP(page);
    await wa.passwordStep(page, user);
    await expect(page.getByText('Use your passkey')).toHaveCount(0);
    expect((await page.request.get(STEP_UP_BEGIN, { maxRedirects: 0 })).status(), 'step-up begin').toBe(404);
    expect((await page.request.post(STEP_UP_FINISH, { maxRedirects: 0 })).status(), 'step-up finish').toBe(404);

    // Sanity guard: the same route exists on a WebAuthn-wired server.
    if (wa.WEBAUTHN_BASE_URL && wa.WEBAUTHN_BASE_URL !== baseURL) {
      const wired = await page.request.get(`${wa.WEBAUTHN_BASE_URL}/login/passkey/begin`);
      expect(wired.status(), 'the wired server must serve passkey login').toBe(200);
    }
  });
});

test.describe('wired server', () => {
  test.skip(!wa.WEBAUTHN_BASE_URL, NO_WEBAUTHN_SERVER);
  if (wa.WEBAUTHN_BASE_URL) test.use({ baseURL: wa.WEBAUTHN_BASE_URL });

  test('T-2.3.1 a passkey registered through a virtual authenticator is listed', async ({ page }) => {
    const { user } = await passkeyMember(page, {
      email: 'pk-register@test.local', displayName: 'PK Register', nickname: 'Hall tablet',
    });
    await expect(wa.deviceNicknameInputs(page)).toHaveCount(1);
    await expect(wa.deviceNicknameInputs(page).first()).toHaveValue('Hall tablet');
    expect(storedNickname(user)).toBe('Hall tablet');
  });

  test('T-2.3.2 a passkey can be renamed, and an empty name falls back to the default', async ({ page }) => {
    const { user } = await passkeyMember(page, { email: 'pk-rename@test.local', displayName: 'PK Rename' });

    await wa.deviceNicknameInputs(page).first().fill('Kitchen tablet');
    const renamed = page.waitForResponse((r) => r.url().endsWith('/rename'));
    await page.click('#webauthn-devices li button:has-text("Save")');
    expect((await renamed).status(), 'rename redirects back to settings').toBe(303);
    await page.goto('/settings');
    await expect(wa.deviceNicknameInputs(page).first()).toHaveValue('Kitchen tablet');

    // Empty and whitespace-only names are not stored as blank labels.
    const rename = `/settings/webauthn/${credentialRowId(user)}/rename`;
    for (const nickname of ['', '   ']) {
      const res = await page.request.post(rename, { form: { csrf_token: await renameCSRFToken(page), nickname }, maxRedirects: 0 });
      expect(res.status(), `rename to ${JSON.stringify(nickname)}`).toBe(303);
      expect(storedNickname(user), 'a blank name must fall back to the default label').toBe('Passkey');
    }
  });

  test('T-2.3.2 [!] renaming a passkey to 10,000 characters is refused', async ({ page }) => {
    test.fail(true, 'DEFECT: passkey nickname has no length cap (service, handler or DB); 10,000 chars are stored');
    const { user } = await passkeyMember(page, { email: 'pk-rename-long@test.local', displayName: 'PK Rename Long' });
    const rename = `/settings/webauthn/${credentialRowId(user)}/rename`;

    // Sanity guard: an ordinary rename on the same route is accepted.
    const ok = await page.request.post(rename, { form: { csrf_token: await renameCSRFToken(page), nickname: 'Laptop' }, maxRedirects: 0 });
    expect(ok.status()).toBe(303);
    expect(storedNickname(user)).toBe('Laptop');

    const res = await page.request.post(rename, {
      form: { csrf_token: await renameCSRFToken(page), nickname: 'n'.repeat(10_000) }, maxRedirects: 0,
    });
    expect(storedNickname(user).length, 'a 10,000-character nickname must not be stored').toBeLessThan(10_000);
    expect(res.status(), 'a 10,000-character nickname must be refused with a 4xx').toBeGreaterThanOrEqual(400);
    expect(res.status()).toBeLessThan(500);
  });

  test('T-2.3.3 a revoked passkey is removed and can no longer sign in', async ({ page }) => {
    const { user } = await passkeyMember(page, { email: 'pk-revoke@test.local', displayName: 'PK Revoke' });

    // Sanity guard: before revocation the passkey signs in.
    await page.context().clearCookies();
    await page.goto('/login');
    expect((await clickPasskeyButton(page, 'Sign in with passkey', LOGIN_FINISH)).status()).toBe(200);
    await page.waitForURL((u) => new URL(u).pathname === '/');

    await page.goto('/settings');
    const revoked = page.waitForResponse((r) => r.url().endsWith('/revoke'));
    await page.click('#webauthn-devices button:has-text("Remove")');
    expect((await revoked).status()).toBe(303);
    await page.waitForLoadState();
    await expect(page.locator('#webauthn-devices')).toContainText('No passkeys registered yet.');
    expect(credentialCount(user)).toBe('0');

    // The authenticator still holds the key; the server must refuse it.
    await page.context().clearCookies();
    await page.goto('/login');
    expect((await clickPasskeyButton(page, 'Sign in with passkey', LOGIN_FINISH)).status(), 'a revoked passkey must be refused').toBe(401);
    await expect(page.locator('[role="alert"]').first()).toContainText('Could not complete passkey verification');
    expect(new URL(page.url()).pathname).toBe('/login');
  });

  test('T-2.3.4 revoking the only passkey of a passkey-only account does not lock it out', async ({ page }) => {
    const { user } = await passkeyMember(page, { email: 'pk-only@test.local', displayName: 'PK Only' });

    // The passkey is this account's only second factor: the password alone
    // is not enough, and the passkey completes the step.
    await wa.passwordStep(page, user);
    expect((await clickPasskeyButton(page, 'Use your passkey', STEP_UP_FINISH)).status()).toBe(200);
    await page.waitForURL((u) => new URL(u).pathname === '/');

    await page.goto('/settings');
    await page.click('#webauthn-devices button:has-text("Remove")');
    await expect(page.locator('#webauthn-devices')).toContainText('No passkeys registered yet.');

    // With no second factor left, the password signs straight in — no
    // dead-end prompt for a factor that no longer exists.
    await page.context().clearCookies();
    expect(await wa.signInWithPassword(page, user), 'the account must still be reachable').toBe('/');
  });

  test('T-2.3.5 passkey login signs the member in', async ({ page }) => {
    await passkeyMember(page, { email: 'pk-login@test.local', displayName: 'PK Login', nickname: 'Login key' });
    await page.context().clearCookies();
    await page.goto('/login');
    expect((await clickPasskeyButton(page, 'Sign in with passkey', LOGIN_FINISH)).status()).toBe(200);
    await page.waitForURL((u) => new URL(u).pathname === '/');
    await expect(page.locator('h1', { hasText: 'Dashboard' })).toBeVisible();

    // Signed in as THIS member: their own device list is on /settings.
    await page.goto('/settings');
    await expect(wa.deviceNicknameInputs(page).first()).toHaveValue('Login key');
  });

  test('T-2.3.6 a passkey satisfies the step-up at /login/mfa', async ({ page }) => {
    const user = wa.seedFreshMember({ email: 'pk-stepup@test.local', displayName: 'PK Step Up' });
    await wa.addVirtualAuthenticator(page);
    expect(await wa.signInWithPassword(page, user)).toBe('/');
    await wa.enrollTOTP(page);
    await wa.registerPasskeyThroughUI(page, 'Step-up key');

    await wa.passwordStep(page, user);
    await expect(page.locator('input[name="code"]'), 'the TOTP prompt is still offered').toBeVisible();
    expect((await clickPasskeyButton(page, 'Use your passkey', STEP_UP_FINISH)).status()).toBe(200);
    await page.waitForURL((u) => new URL(u).pathname === '/');

    // The step-up is FRESH, not merely signed in: a step-up-gated route
    // (passkey registration) is served instead of bouncing to /login/mfa.
    const csrf = await devicesCSRFToken(page);
    const begin = await page.request.post('/settings/webauthn/register/begin', { headers: { 'X-CSRF-Token': csrf }, maxRedirects: 0 });
    expect(begin.status(), 'a passkey step-up must satisfy RequireStepUp').toBe(200);
  });

  test('T-2.3.8 registering with a stale step-up bounces to /login/mfa', async ({ page }) => {
    const user = wa.seedFreshMember({ email: 'pk-stale@test.local', displayName: 'PK Stale' });
    expect(await wa.signInWithPassword(page, user)).toBe('/');
    const { secret } = await wa.enrollTOTP(page);

    // Remember this device, then sign in again through it: the remembered
    // login skips the prompt but is deliberately NOT a fresh step-up.
    let csrf = await wa.passwordStep(page, user);
    const remembered = await page.request.post('/login/mfa', {
      form: { csrf_token: csrf, code: totp.code(secret), remember_device: 'on' }, maxRedirects: 0,
    });
    expect(remembered.status()).toBe(303);
    const cookie = (await page.context().cookies()).find((c) => c.name === 'nestova_remember');
    await page.context().clearCookies();
    await page.context().addCookies([cookie]);
    expect(await wa.signInWithPassword(page, user), 'the remembered device skips the prompt').toBe('/');

    csrf = await devicesCSRFToken(page);
    for (const path of ['/settings/webauthn/register/begin', '/settings/webauthn/register/finish']) {
      const res = await page.request.post(path, { headers: { 'X-CSRF-Token': csrf }, data: {}, maxRedirects: 0 });
      expect(res.status(), `${path} with a stale step-up`).toBe(303);
      expect(res.headers().location).toMatch(/^\/login\/mfa\b/);
    }

    // Sanity guard: after a fresh step-up the same route is served. +1
    // period, because the code for this period was already used above.
    await page.goto('/login/mfa');
    csrf = await page.locator('input[name="csrf_token"]').first().inputValue();
    const stepUp = await page.request.post('/login/mfa', {
      form: { csrf_token: csrf, code: totp.code(secret, { stepOffset: 1 }) }, maxRedirects: 0,
    });
    expect(stepUp.status()).toBe(303);
    const fresh = await page.request.post('/settings/webauthn/register/begin', {
      headers: { 'X-CSRF-Token': await devicesCSRFToken(page) }, maxRedirects: 0,
    });
    expect(fresh.status(), 'a fresh step-up must reach registration').toBe(200);
  });

  test('T-2.3.9 finishing a ceremony that was never begun is 400', async ({ page }) => {
    const { user, credential } = await passkeyMember(page, { email: 'pk-nochallenge@test.local', displayName: 'PK No Challenge' });
    const orphanChallenge = crypto.randomBytes(32).toString('base64url');

    // Login: finish with no begin, then (sanity guard) begin + finish.
    await page.context().clearCookies();
    await page.goto('/login');
    let csrf = await wa.passkeyCSRFToken(page);
    const orphan = await wa.fetchInPage(page, LOGIN_FINISH, {
      method: 'POST', csrf, json: wa.forgeAssertion(credential, { challenge: orphanChallenge }),
    });
    expect(orphan.status, 'login finish with no pending challenge').toBe(400);
    let challenge;
    ({ csrf, challenge } = await beginLogin(page));
    const valid = await wa.fetchInPage(page, LOGIN_FINISH, {
      method: 'POST', csrf, json: wa.forgeAssertion(credential, { challenge, signCount: credential.signCount + 1 }),
    });
    expect(valid.status, 'login finish after begin').toBe(200);

    // Registration: now signed in with a fresh step-up; the earlier UI
    // registration is this route's sanity guard.
    const regCSRF = await devicesCSRFToken(page);
    const reg = await wa.fetchInPage(page, '/settings/webauthn/register/finish', { method: 'POST', csrf: regCSRF, json: {} });
    expect(reg.status, 'registration finish with no pending challenge').toBe(400);

    // Step-up at /login/mfa: finish with no begin, then begin + finish.
    csrf = await wa.passwordStep(page, user);
    const stepOrphan = await wa.fetchInPage(page, STEP_UP_FINISH, {
      method: 'POST', csrf, json: wa.forgeAssertion(credential, { challenge: orphanChallenge, signCount: credential.signCount + 2 }),
    });
    expect(stepOrphan.status, 'step-up finish with no pending challenge').toBe(400);
    const begin = JSON.parse((await wa.fetchInPage(page, STEP_UP_BEGIN)).body);
    const stepValid = await wa.fetchInPage(page, STEP_UP_FINISH, {
      method: 'POST', csrf, json: wa.forgeAssertion(credential, { challenge: begin.publicKey.challenge, signCount: credential.signCount + 3 }),
    });
    expect(stepValid.status, 'step-up finish after begin').toBe(200);
  });

  test('T-2.3.10 a wrong RP ID, a wrong origin or an unknown credential is 401', async ({ page }) => {
    const { credential } = await passkeyMember(page, { email: 'pk-tamper@test.local', displayName: 'PK Tamper' });
    const cases = [
      ['wrong RP ID', { rpId: 'evil.example' }],
      ['wrong origin', { origin: 'http://evil.example' }],
      ['unknown credential', { credentialId: crypto.randomBytes(32).toString('base64url') }],
    ];
    for (const [name, override] of cases) {
      const { csrf, challenge } = await beginLogin(page);
      const res = await wa.fetchInPage(page, LOGIN_FINISH, {
        method: 'POST', csrf, json: wa.forgeAssertion(credential, { challenge, ...override }),
      });
      expect(res.status, name).toBe(401);
    }

    // Sanity guard: the same forged assertion, untampered, signs in.
    const { csrf, challenge } = await beginLogin(page);
    const valid = await wa.fetchInPage(page, LOGIN_FINISH, { method: 'POST', csrf, json: wa.forgeAssertion(credential, { challenge }) });
    expect(valid.status, 'an untampered assertion must be accepted').toBe(200);
  });

  test('T-2.3.11 a WebAuthn body over 64 KiB is 413', async ({ page }) => {
    const { user, credential } = await passkeyMember(page, { email: 'pk-oversize@test.local', displayName: 'PK Oversize' });

    let { csrf } = await beginLogin(page);
    const login413 = await wa.fetchInPage(page, LOGIN_FINISH, { method: 'POST', csrf, rawBody: OVERSIZED_BODY });
    expect(login413.status, 'oversized login finish').toBe(413);

    // Sanity guard: a normal-size body on the same route is accepted.
    let challenge;
    ({ csrf, challenge } = await beginLogin(page));
    const valid = await wa.fetchInPage(page, LOGIN_FINISH, { method: 'POST', csrf, json: wa.forgeAssertion(credential, { challenge }) });
    expect(valid.status).toBe(200);

    // Registration finish (the challenge is popped before the body is read,
    // so begin first).
    const regCSRF = await devicesCSRFToken(page);
    expect((await wa.fetchInPage(page, '/settings/webauthn/register/begin', { method: 'POST', csrf: regCSRF })).status).toBe(200);
    const reg413 = await wa.fetchInPage(page, '/settings/webauthn/register/finish', { method: 'POST', csrf: regCSRF, rawBody: OVERSIZED_BODY });
    expect(reg413.status, 'oversized registration finish').toBe(413);

    ({ csrf } = await beginStepUp(page, user));
    const step413 = await wa.fetchInPage(page, STEP_UP_FINISH, { method: 'POST', csrf, rawBody: OVERSIZED_BODY });
    expect(step413.status, 'oversized step-up finish').toBe(413);
  });

  test('T-2.3.12 replaying a captured assertion is refused', async ({ page }) => {
    await passkeyMember(page, { email: 'pk-replay@test.local', displayName: 'PK Replay' });

    // Capture the assertion the browser itself sends on a real login.
    await page.context().clearCookies();
    await page.goto('/login');
    const sent = page.waitForRequest((r) => new URL(r.url()).pathname === LOGIN_FINISH);
    expect((await clickPasskeyButton(page, 'Sign in with passkey', LOGIN_FINISH)).status(), 'the original login').toBe(200);
    const captured = (await sent).postData();
    await page.waitForURL((u) => new URL(u).pathname === '/');

    const { csrf } = await beginLogin(page);
    const replay = await wa.fetchInPage(page, LOGIN_FINISH, { method: 'POST', csrf, rawBody: captured });
    expect(replay.status, 'a replayed assertion must be refused').toBe(401);
  });

  test('T-2.3.13 a sign-count regression is flagged', async ({ page }) => {
    const { user, credential } = await passkeyMember(page, { email: 'pk-clone@test.local', displayName: 'PK Clone' });

    async function loginWithCount(signCount) {
      const { csrf, challenge } = await beginLogin(page);
      return (await wa.fetchInPage(page, LOGIN_FINISH, {
        method: 'POST', csrf, json: wa.forgeAssertion(credential, { challenge, signCount }),
      })).status;
    }

    // Sanity guard: a rising counter is accepted and not flagged.
    expect(await loginWithCount(10)).toBe(200);
    expect(await loginWithCount(20)).toBe(200);
    expect(signCountAnomalies(user), 'a rising counter must not be flagged').toBe(0);

    // A cloned authenticator reports a LOWER counter than the server has seen.
    const status = await loginWithCount(5);
    expect(status, 'a regression must never be a server error').toBeLessThan(500);
    const flagged = signCountAnomalies(user);
    expect(status >= 400 || flagged > 0, `a regression must be rejected or flagged (status ${status}, flags ${flagged})`).toBe(true);
  });

  test('T-2.3.14 another member\'s credential is 401 with no account enumeration', async ({ page }) => {
    const { user: alice, authenticator, credential: aliceCred } = await passkeyMember(page, {
      email: 'pk-alice@test.local', displayName: 'PK Alice',
    });
    const bob = wa.seedFreshMember({ email: 'pk-bob@test.local', displayName: 'PK Bob' });
    await page.context().clearCookies();
    expect(await wa.signInWithPassword(page, bob)).toBe('/');
    await wa.registerPasskeyThroughUI(page, 'Bob key');
    const bobCred = await credentialOf(authenticator, bob);
    const bobId = wa.b64ToB64url(bobCred.credentialId);
    const unknownId = crypto.randomBytes(32).toString('base64url');

    // Usernameless login.
    const cases = [
      ['Bob\'s credential id under Alice\'s user handle', aliceCred, { credentialId: bobId }],
      ['Bob\'s credential id and handle, signed by Alice', aliceCred, { credentialId: bobId, userHandle: wa.b64ToB64url(bobCred.userHandle) }],
      ['an unknown credential id', aliceCred, { credentialId: unknownId }],
      ['an unknown user handle', aliceCred, { userHandle: crypto.randomBytes(32).toString('base64url') }],
    ];
    const loginBodies = new Set();
    for (const [name, signer, override] of cases) {
      const { csrf, challenge } = await beginLogin(page);
      const res = await wa.fetchInPage(page, LOGIN_FINISH, {
        method: 'POST', csrf, json: wa.forgeAssertion(signer, { challenge, ...override }),
      });
      expect(res.status, name).toBe(401);
      loginBodies.add(res.body);
    }
    expect([...loginBodies], 'every refusal must read the same').toHaveLength(1);

    // Step-up for Alice: Bob's own, perfectly valid assertion is not hers.
    const stepBodies = new Set();
    for (const [name, signer, override] of [
      ['Bob\'s valid assertion', bobCred, {}],
      ['an unknown credential id', aliceCred, { credentialId: unknownId }],
    ]) {
      const { csrf, challenge } = await beginStepUp(page, alice);
      const res = await wa.fetchInPage(page, STEP_UP_FINISH, {
        method: 'POST', csrf, json: wa.forgeAssertion(signer, { challenge, ...override }),
      });
      expect(res.status, `step-up with ${name}`).toBe(401);
      stepBodies.add(res.body);
    }
    expect([...stepBodies], 'every step-up refusal must read the same').toHaveLength(1);

    // Sanity guard: Alice's own assertion passes the same step-up.
    const { csrf, challenge } = await beginStepUp(page, alice);
    const valid = await wa.fetchInPage(page, STEP_UP_FINISH, {
      method: 'POST', csrf, json: wa.forgeAssertion(aliceCred, { challenge, signCount: aliceCred.signCount + 5 }),
    });
    expect(valid.status, 'Alice\'s own passkey must pass her step-up').toBe(200);
  });
});
