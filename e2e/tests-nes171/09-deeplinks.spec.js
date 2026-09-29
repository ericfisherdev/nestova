// §13 QR deep links — zero automated coverage before this run.
//
// A real link only ever appears inside a QR PNG on a kiosk screen, so the
// tests mint their own using the same derivation the server uses:
//   key = HMAC-SHA256(SESSION_SECRET, "nestova:deeplink:v1")
//   sig = base64url-raw(HMAC-SHA256(key, path + "|" + exp))
// That is a deliberate duplication of production logic. It buys the ability to
// test expiry and tampering directly, and if the server's scheme ever changes
// the VALID-link test below fails first, which is the signal to update this.
const { test, expect } = require('@playwright/test');
const crypto = require('crypto');
const { PERSONAS } = require('../tests/fixtures');
const { login } = require('./helpers');
const { psql } = require('../tests/db');

const SECRET = process.env.NESTOVA_SESSION_SECRET || 'dev-only-insecure-session-secret-change-me';
const PURPOSE = 'nestova:deeplink:v1';

function signerKey() {
  return crypto.createHmac('sha256', SECRET).update(PURPOSE).digest();
}

function sign(path, expiryUnix) {
  const mac = crypto.createHmac('sha256', signerKey()).update(`${path}|${expiryUnix}`).digest();
  return mac.toString('base64url');
}

function signedURL(path, { secondsFromNow = 300 } = {}) {
  const exp = Math.floor(Date.now() / 1000) + secondsFromNow;
  return `${path}?exp=${exp}&sig=${encodeURIComponent(sign(path, exp))}`;
}

// A claimable instance in household A that a deep link can act on.
function seedClaimableInstance() {
  const household = psql("SELECT id FROM identity.household WHERE name = 'Household A' LIMIT 1;").trim();
  const owner = psql(
    `SELECT id FROM identity.member WHERE household_id = '${household}' AND role = 'owner' LIMIT 1;`,
  ).trim();
  const taskId = psql(`
    INSERT INTO nestova.recurring_task
      (id, household_id, title, category, cadence, rotation_policy, points, lead_time_days, active, created_at, updated_at)
    VALUES (gen_random_uuid(), '${household}', 'Deep link probe', 'chore',
            '{"Freq":"daily","Interval":1,"Anchor":"2026-08-25T00:00:00Z","ByWeekday":null}'::jsonb,
            'claimable', 5, 0, true, now(), now())
    RETURNING id;`).trim();
  return psql(`
    INSERT INTO nestova.task_instance
      (id, recurring_task_id, household_id, assignee_id, due_on, status, kind, created_at, updated_at)
    VALUES (gen_random_uuid(), '${taskId}', '${household}', '${owner}', current_date, 'pending', 'scheduled', now(), now())
    RETURNING id;`).trim();
}

async function statusOf(page, path) {
  const res = await page.request.get(path, { maxRedirects: 0 });
  return res.status();
}

test.describe('§13 deep links', () => {
  test('T-13.1.1 a validly signed link opens its action screen', async ({ page }) => {
    const id = seedClaimableInstance();
    await login(page, PERSONAS.owner);
    const status = await statusOf(page, signedURL(`/go/claim-task/${id}`));
    expect(status, 'a validly signed link must be accepted').toBe(200);
  });

  test('T-13.2.2 a tampered signature is refused', async ({ page }) => {
    const id = seedClaimableInstance();
    await login(page, PERSONAS.owner);
    const good = signedURL(`/go/claim-task/${id}`);
    const tampered = good.replace(/sig=([^&]+)/, (m, sig) => `sig=${sig.slice(0, -3)}AAA`);
    const status = await statusOf(page, tampered);
    expect(status, 'a tampered signature must be refused').toBeGreaterThanOrEqual(400);
  });

  test('T-13.2.3 swapping the id under a valid signature is refused', async ({ page }) => {
    const id = seedClaimableInstance();
    const other = seedClaimableInstance();
    await login(page, PERSONAS.owner);

    // Sign one id, then present another: the signature covers the path.
    const exp = Math.floor(Date.now() / 1000) + 300;
    const sig = sign(`/go/claim-task/${id}`, exp);
    const swapped = `/go/claim-task/${other}?exp=${exp}&sig=${encodeURIComponent(sig)}`;
    expect(await statusOf(page, swapped), 'a signature must not transfer to another id').toBeGreaterThanOrEqual(400);
  });

  test('T-13.2.1 an expired link is refused', async ({ page }) => {
    const id = seedClaimableInstance();
    await login(page, PERSONAS.owner);
    const expired = signedURL(`/go/claim-task/${id}`, { secondsFromNow: -60 });
    expect(await statusOf(page, expired), 'an expired link must be refused').toBeGreaterThanOrEqual(400);
  });

  test('T-13.2.1b extending the expiry without re-signing is refused', async ({ page }) => {
    const id = seedClaimableInstance();
    await login(page, PERSONAS.owner);
    const path = `/go/claim-task/${id}`;
    const exp = Math.floor(Date.now() / 1000) - 60;
    const sig = sign(path, exp);
    // Same signature, a far-future exp: the MAC covers the expiry too.
    const forged = `${path}?exp=${Math.floor(Date.now() / 1000) + 9999}&sig=${encodeURIComponent(sig)}`;
    expect(await statusOf(page, forged), 'the expiry must be covered by the signature').toBeGreaterThanOrEqual(400);
  });

  test('T-13.2.4 an unknown action is refused', async ({ page }) => {
    const id = seedClaimableInstance();
    await login(page, PERSONAS.owner);
    expect(await statusOf(page, signedURL(`/go/detonate/${id}`))).toBeGreaterThanOrEqual(400);
  });

  test('T-13.6 an unsigned link is refused', async ({ page }) => {
    const id = seedClaimableInstance();
    await login(page, PERSONAS.owner);
    expect(await statusOf(page, `/go/claim-task/${id}`), 'a link with no signature must be refused').toBeGreaterThanOrEqual(400);
  });

  test('T-13.1.3 an unauthenticated scan is sent to login, not refused outright', async ({ browser }) => {
    const id = seedClaimableInstance();
    const anon = await browser.newContext();
    const page = await anon.newPage();
    const status = await statusOf(page, signedURL(`/go/claim-task/${id}`));
    // A signed-but-unauthenticated scan should hand off to login (303) so the
    // member can continue to the action, not dead-end with a 401.
    expect([303, 302], `expected a login hand-off, got ${status}`).toContain(status);
    await anon.close();
  });
});
