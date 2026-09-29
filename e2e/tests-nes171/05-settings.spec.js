// §3 Settings — notification contact, quiet hours, and member PIN.
//
// Zero automated coverage before this run. These are validation-heavy and are
// exactly the "prevent entering data the system cannot use" surface NES-171
// exists to probe.
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, postForm } = require('./helpers');

async function settingsToken(page) {
  await page.goto('/settings');
  return page.locator('input[name="csrf_token"]').first().inputValue();
}

test.describe('§3.1 notification contact', () => {
  test('T-3.1.1 a valid phone number is accepted', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const csrf_token = await settingsToken(page);
    const status = await postForm(page, '/settings/notify/phone', { csrf_token, phone: '+15555550100' });
    expect(status).toBe(200);
  });

  test('T-3.1.2 malformed phone numbers are refused', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const csrf_token = await settingsToken(page);
    const bad = ['123', 'abc', '+', '5'.repeat(40), 'not a phone', '+1 (555) 010-9999x'];
    const accepted = [];
    for (const phone of bad) {
      const status = await postForm(page, '/settings/notify/phone', { csrf_token, phone });
      if (status !== 400) accepted.push(`${phone.slice(0, 12)} -> ${status}`);
    }
    expect(accepted, 'malformed phone numbers that were not refused with 400').toEqual([]);
  });

  test('T-3.1.3 SMS opt-in with no phone on file is refused', async ({ page }) => {
    await login(page, PERSONAS.child); // child has never set a phone
    const csrf_token = await settingsToken(page);
    const status = await postForm(page, '/settings/notify/opt-in', { csrf_token, opted_in: 'on' });
    expect(status, 'opting in to SMS without a phone must be refused').toBe(400);
  });
});

test.describe('§3.2 quiet hours', () => {
  // Guards the payload: if a VALID window stops being accepted, the refusal
  // assertions below would pass for the wrong reason.
  test('sanity: a valid quiet-hours window is accepted', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const csrf_token = await settingsToken(page);
    const status = await postForm(page, '/settings/notify/quiet-hours', {
      csrf_token, quiet_enabled: 'on', quiet_start: '22:00', quiet_end: '07:00',
    });
    expect(status, 'a valid window must be accepted').toBe(200);
  });

  test('T-3.2.1 the owner can set a valid window', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const csrf_token = await settingsToken(page);
    const status = await postForm(page, '/settings/notify/quiet-hours', { csrf_token, quiet_enabled: 'on', quiet_start: '22:00', quiet_end: '07:00' });
    expect(status).toBe(200);
  });

  test('T-3.2.2 a half-filled window is refused', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const csrf_token = await settingsToken(page);
    const startOnly = await postForm(page, '/settings/notify/quiet-hours', { csrf_token, quiet_enabled: 'on', quiet_start: '22:00', quiet_end: '' });
    const endOnly = await postForm(page, '/settings/notify/quiet-hours', { csrf_token, quiet_enabled: 'on', quiet_start: '', quiet_end: '07:00' });
    expect({ startOnly, endOnly }).toEqual({ startOnly: 400, endOnly: 400 });
  });

  test('T-3.2.5 out-of-range times are refused', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const csrf_token = await settingsToken(page);
    const bad = [['25:00', '07:00'], ['12:60', '07:00'], ['-1:00', '07:00'], ['noon', '07:00']];
    const accepted = [];
    for (const [start, end] of bad) {
      const status = await postForm(page, '/settings/notify/quiet-hours', { csrf_token, quiet_enabled: 'on', quiet_start: start, quiet_end: end });
      if (status !== 400) accepted.push(`${start} -> ${status}`);
    }
    expect(accepted, 'invalid times that were not refused with 400').toEqual([]);
  });
});

test.describe('§3.3 member PIN', () => {
  test('T-3.3.1 a 4-digit PIN is accepted', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const csrf_token = await settingsToken(page);
    expect(await postForm(page, '/settings/pin', { csrf_token, pin: '1234' })).toBe(200);
  });

  test('T-3.3.2 PIN length bounds are enforced', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const csrf_token = await settingsToken(page);
    const eight = await postForm(page, '/settings/pin', { csrf_token, pin: '12345678' });
    const three = await postForm(page, '/settings/pin', { csrf_token, pin: '123' });
    const nine = await postForm(page, '/settings/pin', { csrf_token, pin: '123456789' });
    expect({ eight, three, nine }).toEqual({ eight: 200, three: 400, nine: 400 });
  });

  test('T-3.3.3 a non-numeric PIN is refused', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const csrf_token = await settingsToken(page);
    const bad = ['abcd', '12 34', '12.34', '١٢٣٤', '12​34'];
    const accepted = [];
    for (const pin of bad) {
      const status = await postForm(page, '/settings/pin', { csrf_token, pin });
      if (status !== 400) accepted.push(`${JSON.stringify(pin)} -> ${status}`);
    }
    expect(accepted, 'non-numeric PINs that were not refused with 400').toEqual([]);
  });

  test('T-3.3.4 clearing a PIN works', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const csrf_token = await settingsToken(page);
    await postForm(page, '/settings/pin', { csrf_token, pin: '4321' });
    expect(await postForm(page, '/settings/pin/clear', { csrf_token })).toBe(200);
  });

  test('T-3.3.5 a child cannot set another member\'s PIN', async ({ page }) => {
    const { psql } = require('../tests/db');
    const ownerId = psql("SELECT id FROM identity.member WHERE display_name = 'Owner A' LIMIT 1;").trim();
    await login(page, PERSONAS.child);
    const csrf_token = await settingsToken(page);
    const status = await postForm(page, `/settings/members/${ownerId}/pin`, { csrf_token, pin: '9999' });
    expect(status, "a child must not set another member's PIN").toBe(403);
  });
});
