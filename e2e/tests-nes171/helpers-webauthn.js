// Second-factor helpers for the §2.2 TOTP and §2.3 passkey specs.
//
// The TOTP half (seedFreshMember, enrollTOTP, passwordStep,
// waitForTOTPWindowRoom) is shared by 12-mfa, 13-mfa-recovery and
// 25-passkeys: the passkey step-up items need a TOTP-enrolled member too.
//
// The WebAuthn half is two tools, used together:
//
//   1. A Chromium CDP virtual authenticator, so the REAL page scripts
//      (webauthn-register.js, login-passkey.js) run navigator.credentials
//      end to end with no physical device.
//   2. A forged-assertion builder. The virtual authenticator only ever
//      produces well-formed assertions for the page's own origin; the
//      negative items (wrong RP ID, wrong origin, unknown credential,
//      sign-count regression, another member's credential) need a response a
//      real authenticator would never produce. The builder signs one with the
//      credential's private key, read back from the virtual authenticator.
//
// Passkeys need a server with WebAuthn WIRED, which happens only when
// PUBLIC_BASE_URL is set (cmd/server/main.go). The default checklist server
// does not set it, so these specs target NESTOVA_WEBAUTHN_BASE_URL: a second
// server on the same database, started with PUBLIC_BASE_URL equal to its own
// origin. When that variable is unset, the passkey specs skip themselves.
const crypto = require('crypto');
const { expect } = require('@playwright/test');
const { PERSONAS, PASSWORD } = require('../tests/fixtures');
const { psql, seedMemberInA } = require('../tests/db');
const totp = require('./totp');

const WEBAUTHN_BASE_URL = process.env.NESTOVA_WEBAUTHN_BASE_URL || '';

// addVirtualAuthenticator attaches a platform-style CTAP2 authenticator to
// page: resident keys (discoverable, so usernameless login works) and
// successful user verification (the server REQUIRES UV, main.go).
async function addVirtualAuthenticator(page) {
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('WebAuthn.enable');
  const { authenticatorId } = await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: {
      protocol: 'ctap2',
      transport: 'internal',
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    },
  });
  return { cdp, authenticatorId };
}

// storedCredentials returns every credential the virtual authenticator holds,
// with its private key (PKCS#8, base64) and sign count.
async function storedCredentials({ cdp, authenticatorId }) {
  const { credentials } = await cdp.send('WebAuthn.getCredentials', { authenticatorId });
  return credentials;
}

// signInWithPassword submits the password form and returns the pathname it
// lands on — "/" for a member with no second factor, "/login/mfa" for one
// with TOTP or a passkey.
async function signInWithPassword(page, persona) {
  await page.goto('/login');
  await page.fill('input[name="email"]', persona.email);
  await page.fill('input[name="password"]', persona.password);
  // type=submit: on this server the passkey button ("Sign in with passkey")
  // also matches a bare has-text("Sign in"), and comes first.
  await page.click('button[type="submit"]:has-text("Sign in")');
  await page.waitForURL((u) => ['/', '/login/mfa'].includes(new URL(u).pathname), { timeout: 15_000 });
  return new URL(page.url()).pathname;
}

// registerPasskeyThroughUI drives the real "Add a passkey" flow on /settings
// and waits for the reloaded page to list the new nickname.
async function registerPasskeyThroughUI(page, nickname) {
  await page.goto('/settings');
  await page.click('button:has-text("Add a passkey")');
  await page.fill('#webauthn-new-nickname', nickname);
  await Promise.all([
    page.waitForEvent('load'),
    page.click('#webauthn-devices form button[type="submit"]'),
  ]);
  await expect(deviceNicknameInputs(page).first()).toBeVisible();
}

// deviceNicknameInputs locates the rename inputs of the "Your devices" list,
// one per registered passkey.
function deviceNicknameInputs(page) {
  return page.locator('#webauthn-devices input[name="nickname"]');
}

// passkeyCSRFToken reads the CSRF token the passkey button carries on the
// current page (the JSON endpoints take it in X-CSRF-Token, not a form field).
async function passkeyCSRFToken(page) {
  return page.locator('[x-data="passkeyAssertion"]').first().getAttribute('data-csrf-token');
}

// fetchInPage issues a request from inside the browser session and returns
// { status, body }. Redirects are not followed, so a step-up bounce shows up
// as 303 (an opaque manual redirect) instead of being hidden.
async function fetchInPage(page, path, { method = 'GET', csrf, json, rawBody } = {}) {
  return page.evaluate(async ({ path, method, csrf, json, rawBody }) => {
    const headers = {};
    if (csrf) headers['X-CSRF-Token'] = csrf;
    let body;
    if (json !== undefined) {
      headers['Content-Type'] = 'application/json';
      body = JSON.stringify(json);
    } else if (rawBody !== undefined) {
      headers['Content-Type'] = 'application/json';
      body = rawBody;
    }
    const res = await fetch(path, { method, headers, body, redirect: 'manual' });
    if (res.type === 'opaqueredirect') return { status: 303, body: '' };
    return { status: res.status, body: await res.text() };
  }, { path, method, csrf, json, rawBody });
}

// b64ToB64url converts CDP's standard base64 "binary" values to the
// base64url encoding WebAuthn JSON uses.
function b64ToB64url(b64) {
  return Buffer.from(b64, 'base64').toString('base64url');
}

// forgeAssertion builds a PublicKeyCredential.toJSON()-shaped assertion
// signed with credential.privateKey. Every field a negative test varies is a
// parameter; the defaults describe a VALID assertion for the WebAuthn server,
// so each negative test changes exactly one thing.
function forgeAssertion(credential, {
  challenge,
  origin = WEBAUTHN_BASE_URL,
  rpId = new URL(WEBAUTHN_BASE_URL).hostname,
  signCount = credential.signCount + 1,
  credentialId = b64ToB64url(credential.credentialId),
  userHandle = b64ToB64url(credential.userHandle),
  flags = 0x05, // UP | UV
}) {
  const counter = Buffer.alloc(4);
  counter.writeUInt32BE(signCount);
  const authenticatorData = Buffer.concat([
    crypto.createHash('sha256').update(rpId).digest(),
    Buffer.from([flags]),
    counter,
  ]);
  const clientDataJSON = Buffer.from(JSON.stringify({
    type: 'webauthn.get', challenge, origin, crossOrigin: false,
  }));
  const key = crypto.createPrivateKey({
    key: Buffer.from(credential.privateKey, 'base64'), format: 'der', type: 'pkcs8',
  });
  const signature = crypto.sign('sha256', Buffer.concat([
    authenticatorData, crypto.createHash('sha256').update(clientDataJSON).digest(),
  ]), key);

  return {
    id: credentialId,
    rawId: credentialId,
    type: 'public-key',
    response: {
      clientDataJSON: clientDataJSON.toString('base64url'),
      authenticatorData: authenticatorData.toString('base64url'),
      signature: signature.toString('base64url'),
      userHandle,
    },
    clientExtensionResults: {},
  };
}

// seedFreshMember seeds (or reuses) a household-A member and strips every
// second factor and notification it may carry from a previous run, so each
// test starts from a password-only account. Returns the persona plus its id.
function seedFreshMember({ email, displayName, role = 'adult' }) {
  const id = seedMemberInA({ displayName, email, role, copyHashFrom: PERSONAS.owner.email });
  psql(`
    DELETE FROM identity.member_mfa WHERE member_id = '${id}';
    DELETE FROM identity.member_credential WHERE member_id = '${id}';
    DELETE FROM nestova.notification WHERE member_id = '${id}';`);
  return { id, email, password: PASSWORD, displayName };
}

// postForReveal POSTs a form through the page's cookie jar and returns the
// status and HTML. The one-time reveals (TOTP secret, recovery codes) are
// then parsed from the HTML in Node: an earlier version swapped the page body
// for the response inside page.evaluate, and a navigation landing mid-call
// intermittently destroyed that execution context.
async function postForReveal(page, path, fields) {
  const res = await page.request.post(path, { form: fields, maxRedirects: 0 });
  return { status: res.status(), html: await res.text() };
}

// revealedSecret reads the manual-entry TOTP secret from an enrolment reveal.
function revealedSecret(html) {
  const input = html.match(/<input[^>]*id="mfa-manual-secret"[^>]*>/);
  const value = input && input[0].match(/value="([^"]*)"/);
  return value ? value[1] : '';
}

// revealedRecoveryCodes reads the recovery codes a confirm or regenerate
// response revealed.
function revealedRecoveryCodes(html) {
  return [...html.matchAll(/<li[^>]*>\s*([A-Za-z0-9-]{6,})\s*<\/li>/g)].map((m) => m[1]);
}

// enrollTOTP enrols and confirms TOTP for the member signed in on page and
// returns the secret and the first batch of recovery codes. The session stays
// signed in.
async function enrollTOTP(page) {
  await page.goto('/settings');
  const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
  const enrol = await postForReveal(page, '/settings/mfa/enroll', { csrf_token });
  expect(enrol.status).toBe(200);
  const secret = revealedSecret(enrol.html);
  expect(secret, 'enrolment must reveal a manual-entry secret').toBeTruthy();
  const confirm = await postForReveal(page, '/settings/mfa/confirm', { csrf_token, code: totp.code(secret) });
  expect(confirm.status, 'confirming with the current code must succeed').toBe(200);
  const recoveryCodes = revealedRecoveryCodes(confirm.html);
  expect(recoveryCodes.length, 'confirmation must reveal recovery codes').toBeGreaterThan(0);
  return { secret, recoveryCodes };
}

// passwordStep signs out, submits the password form, asserts the member is
// sent to the second-factor page, and returns that page's CSRF token.
async function passwordStep(page, persona) {
  await page.context().clearCookies();
  expect(await signInWithPassword(page, persona), 'an enrolled member must be sent to /login/mfa').toBe('/login/mfa');
  return page.locator('input[name="csrf_token"]').first().inputValue();
}

// waitForTOTPWindowRoom waits until at least `seconds` remain in the current
// 30-second TOTP period, so a code computed now is still in the same period
// when the server checks it. Without this, a period boundary between
// computing and verifying turns a "-1 window" code into a "-2 window" one.
async function waitForTOTPWindowRoom(page, seconds) {
  const remaining = 30 - ((Date.now() / 1000) % 30);
  if (remaining < seconds) await page.waitForTimeout(remaining * 1000 + 250);
}

module.exports = {
  seedFreshMember,
  postForReveal,
  revealedRecoveryCodes,
  enrollTOTP,
  passwordStep,
  waitForTOTPWindowRoom,
  WEBAUTHN_BASE_URL,
  addVirtualAuthenticator,
  storedCredentials,
  signInWithPassword,
  registerPasskeyThroughUI,
  deviceNicknameInputs,
  passkeyCSRFToken,
  fetchInPage,
  b64ToB64url,
  forgeAssertion,
};
