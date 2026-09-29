// §12 Kiosk — zero automated coverage before this run.
//
// A kiosk device holds its own identity, separate from a member login, so most
// of these run in a context with NO member session: that is the state a real
// entryway screen is in.
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, postForm } = require('./helpers');
const { psql } = require('../tests/db');

const KIOSK_PAGES = ['/kiosk/chores', '/kiosk/meals', '/kiosk/shopping', '/kiosk/calendar', '/kiosk/photos'];

// generateCode asks the app for an activation code as a parent and returns the
// plaintext code. The code is only ever shown once, in the response, so it is
// scraped from the rendered page rather than read from the database (which
// stores a hash).
async function generateCode(page, name = 'Test kiosk') {
  await login(page, PERSONAS.owner);
  await page.goto('/settings');
  const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();

  // The reveal fragment is the only place the plaintext code exists — the
  // server keeps a hash — and it carries it in an input value, not in text.
  await page.evaluate(async ({ csrf_token, name }) => {
    const body = new URLSearchParams({ csrf_token, name }).toString();
    const res = await fetch('/settings/kiosk/generate', {
      method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded' }, body,
    });
    document.body.innerHTML = await res.text();
  }, { csrf_token, name });

  const code = await page.locator('#kiosk-code-value').inputValue();
  expect(code, 'the generate response should reveal an activation code').toBeTruthy();
  return code;
}

// statusOf fetches a page for its status alone. page.goto() throws on some 4xx
// responses, which would mask exactly the refusals these tests assert.
async function statusOf(page, path) {
  const res = await page.request.get(path, { maxRedirects: 0 });
  return res.status();
}

test.describe('§12 kiosk', () => {
  test('T-12.1.1 a valid code activates a device and the views render', async ({ page, browser }) => {
    const code = await generateCode(page, 'Entryway');

    // A fresh context with no member session: the device's own identity is
    // what must carry these pages.
    const device = await browser.newContext();
    const kiosk = await device.newPage();

    await kiosk.goto(`/kiosk/activate?code=${encodeURIComponent(code)}`);
    const csrf_token = await kiosk.locator('input[name="csrf_token"]').first().inputValue();
    const status = await postForm(kiosk, '/kiosk/activate', { csrf_token, code });
    expect(status, 'a valid activation code must be accepted').toBe(303);

    const failures = [];
    for (const path of KIOSK_PAGES) {
      const status = await statusOf(kiosk, path);
      if (status !== 200) failures.push(`${path} -> ${status}`);
    }
    expect(failures, 'kiosk pages that did not render for an activated device').toEqual([]);
    await device.close();
  });

  test('T-12.2.1 unknown, expired and reused codes are refused', async ({ page, browser }) => {
    const code = await generateCode(page, 'Reuse probe');

    const device = await browser.newContext();
    const kiosk = await device.newPage();
    await kiosk.goto('/kiosk/activate');
    const csrf_token = await kiosk.locator('input[name="csrf_token"]').first().inputValue();

    const unknown = await postForm(kiosk, '/kiosk/activate', { csrf_token, code: 'ZZZZZZZZ' });
    expect(unknown, 'an unknown code must be refused').toBe(401);

    // Consume the real code, then replay it.
    const first = await postForm(kiosk, '/kiosk/activate', { csrf_token, code });
    expect(first, 'the first use of a valid code must succeed').toBe(303);

    const replay = await browser.newContext();
    const kiosk2 = await replay.newPage();
    await kiosk2.goto('/kiosk/activate');
    const token2 = await kiosk2.locator('input[name="csrf_token"]').first().inputValue();
    const reused = await postForm(kiosk2, '/kiosk/activate', { csrf_token: token2, code });
    expect(reused, 'an already-used code must not activate a second device').toBe(401);

    await device.close();
    await replay.close();
  });

  test('T-12.2.1b an expired code is refused', async ({ page, browser }) => {
    const code = await generateCode(page, 'Expiry probe');
    // Activation codes live 15 minutes; age this one past that rather than wait.
    psql("UPDATE nestova.kiosk_activation_code SET expires_at = now() - interval '1 minute' WHERE used_at IS NULL;");

    const device = await browser.newContext();
    const kiosk = await device.newPage();
    await kiosk.goto('/kiosk/activate');
    const csrf_token = await kiosk.locator('input[name="csrf_token"]').first().inputValue();
    const status = await postForm(kiosk, '/kiosk/activate', { csrf_token, code });
    expect(status, 'an expired code must be refused').toBe(401);
    await device.close();
  });

  test('T-12.2.2 kiosk pages refuse a device with no identity', async ({ browser }) => {
    const anon = await browser.newContext();
    const kiosk = await anon.newPage();
    const leaked = [];
    for (const path of KIOSK_PAGES) {
      if (await statusOf(kiosk, path) === 200) leaked.push(path);
    }
    expect(leaked, 'kiosk pages that rendered without a device identity').toEqual([]);
    await anon.close();
  });

  test('T-12.2.3 revoking a device takes effect immediately', async ({ page, browser }) => {
    const code = await generateCode(page, 'Revoke probe');

    const device = await browser.newContext();
    const kiosk = await device.newPage();
    await kiosk.goto('/kiosk/activate');
    const activateToken = await kiosk.locator('input[name="csrf_token"]').first().inputValue();
    expect(await postForm(kiosk, '/kiosk/activate', { csrf_token: activateToken, code })).toBe(303);
    expect(await statusOf(kiosk, '/kiosk/chores'), 'the device should work before revocation').toBe(200);

    const deviceId = psql(
      "SELECT id FROM nestova.kiosk_device ORDER BY created_at DESC LIMIT 1;",
    ).trim();
    await page.goto('/settings');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
    await postForm(page, `/settings/kiosk/${deviceId}/revoke`, { csrf_token });

    expect(await statusOf(kiosk, '/kiosk/chores'), 'a revoked device must lose access at once').toBe(401);
    await device.close();
  });

  test('T-12.2.4 a kiosk device cannot reach member-only routes', async ({ page, browser }) => {
    const code = await generateCode(page, 'Scope probe');

    const device = await browser.newContext();
    const kiosk = await device.newPage();
    await kiosk.goto('/kiosk/activate');
    const csrf_token = await kiosk.locator('input[name="csrf_token"]').first().inputValue();
    expect(await postForm(kiosk, '/kiosk/activate', { csrf_token, code })).toBe(303);

    const leaked = [];
    for (const path of ['/settings', '/admin/rewards', '/members/new', '/tasks/new']) {
      if (await statusOf(kiosk, path) === 200) leaked.push(path);
    }
    expect(leaked, 'member-only routes a kiosk device could reach').toEqual([]);
    await device.close();
  });
});
