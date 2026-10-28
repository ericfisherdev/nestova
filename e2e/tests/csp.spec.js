// E2E guard for the strict Content-Security-Policy (NES-212).
//
// Nestova enforces `script-src 'self'; style-src 'self'` with no
// 'unsafe-inline', 'unsafe-eval', nonces or hashes (internal/platform/httpserver
// /csp.go). There is no Report-Only phase and no report collector on a LAN
// appliance, so this spec IS the violation reporter: it visits every page,
// drives the Alpine- and HTMX-powered controls on it, and fails on any
// securitypolicyviolation event or "Content Security Policy" console error.
// "Zero violations" alone proves little if a blocked script simply left a
// control inert, so each page also exercises a control that depends on the
// scripts the policy could have broken.
//
// Specs share the suite's logged-in storageState unless a describe block says
// otherwise. The settings/MFA/login-MFA flows use a dedicated, disposable
// member because enrolling MFA changes every later login for that account.
//
// The passkey half of the settings test needs WebAuthn wired, which the
// server does only when PUBLIC_BASE_URL is set (cmd/server/main.go); without
// it the passkey section is not rendered and that one test skips itself.
const { test: base, expect } = require('@playwright/test');
const fs = require('fs');
const { psql } = require('./db');
const crypto = require('crypto');
const { requireEnv } = require('./env');
const { watchCSP } = require('./csp-watch');
const { writeTempPng } = require('./png');
const totp = require('../tests-nes171/totp');

const STRICT_SCRIPT_SRC = "script-src 'self'";
const MEMBER_PASSWORD = 'testtest';

// test extends the base with `csp`, a watcher on the default page that also
// collects the watchers of any extra pages a test opens, and asserts every one
// of them is clean when the test body returns. A test that needs an earlier
// checkpoint calls csp.expectNone(label) itself.
const test = base.extend({
  csp: async ({ page }, use) => {
    const watchers = [{ label: 'main page', watcher: await watchCSP(page) }];
    await use({
      watch: async (extraPage, label) => {
        const watcher = await watchCSP(extraPage);
        watchers.push({ label, watcher });
        return watcher;
      },
      expectNone: (label) => watchers[0].watcher.expectNone(label),
    });
    for (const { label, watcher } of watchers) watcher.expectNone(label);
  },
});

// visit navigates to path, waits for the page to settle and checks the
// response really carried the enforcing header (a Report-Only or missing
// header would make "zero violations" meaningless: nothing would be blocked).
async function visit(page, path) {
  const response = await page.goto(path);
  expect(response, `no response for ${path}`).not.toBeNull();
  const policy = response.headers()['content-security-policy'] ?? '';
  expect(policy, `${path} must send an enforcing Content-Security-Policy`).toContain(STRICT_SCRIPT_SRC);
  await page.waitForLoadState('networkidle');
  return response;
}

// ownerHouseholdSQL selects the household of the suite's logged-in owner.
function ownerHouseholdSQL() {
  const email = requireEnv('NESTOVA_EMAIL');
  return `(SELECT household_id FROM identity.member WHERE email = '${email}' LIMIT 1)`;
}

test.describe('dashboard and shell', () => {
  test('the dashboard renders with zero violations', async ({ page, csp }) => {
    await visit(page, '/');
    await expect(page.getByRole('heading', { name: 'Family' })).toBeVisible();
    csp.expectNone('dashboard');
  });

  test('the service worker registers under the policy', async ({ page, csp }) => {
    await visit(page, '/');
    const scope = await page.evaluate(async () => (await navigator.serviceWorker.ready).scope);
    expect(new URL(scope).pathname).toBe('/');
    csp.expectNone('service worker registration');
  });

  test.describe('narrow-screen drawer', () => {
    test.use({ viewport: { width: 390, height: 800 } });

    test('the nav toggle opens the drawer, Escape closes it and focus returns', async ({ page, csp }) => {
      await visit(page, '/');
      const toggle = page.getByRole('button', { name: 'Open navigation' });
      const drawer = page.locator('#sidebar');

      await expect(toggle).toHaveAttribute('aria-expanded', 'false');
      await toggle.click();
      await expect(toggle).toHaveAttribute('aria-expanded', 'true');
      await expect(drawer).toBeFocused();

      await page.keyboard.press('Escape');
      await expect(toggle).toHaveAttribute('aria-expanded', 'false');
      await expect(toggle).toBeFocused();
      csp.expectNone('nav drawer');
    });
  });
});

test.describe('tasks', () => {
  const TS = Date.now();
  const titles = { countdown: `E2E CSP Countdown ${TS}`, claim: `E2E CSP Claim ${TS}` };

  test.beforeAll(() => {
    // One claimed-with-expiry instance (renders the claim countdown and, with
    // a before/after photo policy, both proof-photo slots) and one unclaimed
    // instance (renders the Claim button). Seeded directly because task
    // instances are only materialized by a 5-minute background scheduler.
    psql(`
      WITH ins_rt AS (
        INSERT INTO recurring_task
          (id, household_id, title, category, cadence, rotation_policy, points, lead_time_days, active, photo_policy)
        SELECT gen_random_uuid(), ${ownerHouseholdSQL()}, t.title, 'chore',
               ('{"Freq":"monthly","Anchor":"' || to_char(CURRENT_DATE, 'YYYY-MM-DD') || 'T00:00:00Z","Interval":1,"ByWeekday":null}')::jsonb,
               'claimable', 5, 0, true, t.photo_policy
        FROM (VALUES ('${titles.countdown}', 'before_after'), ('${titles.claim}', 'none')) AS t(title, photo_policy)
        RETURNING id, household_id, title
      ), owner AS (
        SELECT id FROM identity.member WHERE email = '${requireEnv('NESTOVA_EMAIL')}' LIMIT 1
      )
      INSERT INTO task_instance
        (id, household_id, recurring_task_id, assignee_id, due_on, status, claimed_by, claimed_at, claim_expires_at)
      SELECT gen_random_uuid(), r.household_id, r.id,
             CASE WHEN r.title = '${titles.countdown}' THEN owner.id END,
             CURRENT_DATE, 'pending',
             CASE WHEN r.title = '${titles.countdown}' THEN owner.id END,
             CASE WHEN r.title = '${titles.countdown}' THEN now() END,
             CASE WHEN r.title = '${titles.countdown}' THEN now() + interval '10 hours' END
      FROM ins_rt r, owner;
    `);
  });

  test.afterAll(() => {
    psql(`DELETE FROM recurring_task WHERE title IN ('${titles.countdown}', '${titles.claim}');`);
  });

  const row = (page, title) => page.locator('#task-groups [id^="task-"]').filter({ hasText: title }).first();

  test('the list renders the claim countdown and a proof photo auto-submits', async ({ page, csp }) => {
    await visit(page, '/tasks');

    // The countdown is an Alpine component (claim-countdown.js) that fills its
    // badge from a data attribute; a blocked or unparsed component leaves the
    // badge empty.
    const claimed = row(page, titles.countdown);
    await expect(claimed).toBeVisible();
    await expect(claimed.locator('[x-text="label"]')).toHaveText(/^expires in \d+h \d+m$/);

    // Choosing a photo submits its form without a button (auto-submit.js
    // replaced an inline onchange, which the policy blocks).
    const upload = page.waitForRequest((req) => req.method() === 'POST' && /\/tasks\/[^/]+\/photos$/.test(req.url()));
    const jpeg = Buffer.from(
      '/9j/4AAQSkZJRgABAQEASABIAAD/2wBDAP//////////////////////////////////////////////////////////////////////////////////////wgALCAABAAEBAREA/8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQABPxA=',
      'base64',
    );
    await claimed.getByTestId('proof-photo-input-before').setInputFiles({
      name: 'before.jpg', mimeType: 'image/jpeg', buffer: jpeg,
    });
    await upload;
    await page.waitForLoadState('networkidle');
    csp.expectNone('tasks list');
  });

  test('a claimable row can be claimed through htmx', async ({ page, csp }) => {
    await visit(page, '/tasks');
    const unclaimed = row(page, titles.claim);
    await unclaimed.getByRole('button', { name: 'Claim' }).click();
    await expect(row(page, titles.claim).getByRole('button', { name: 'Done' })).toBeVisible();
    csp.expectNone('claim swap');
  });

  test('the add-task form reveals the weekday picker (Alpine taskForm)', async ({ page, csp }) => {
    await visit(page, '/tasks/new');
    const sunday = page.locator('input[name="byweekday"][value="0"]');
    await expect(sunday).toBeHidden();
    await page.locator('select[name="freq"]').selectOption('weekly');
    await expect(sunday).toBeVisible();
    csp.expectNone('add-task form');
  });
});

test.describe('groceries', () => {
  test('the page renders and adding a shopping item swaps in over htmx', async ({ page, csp }) => {
    await visit(page, '/groceries');
    await expect(page.getByRole('heading', { name: 'Shopping list', level: 2 })).toBeVisible();

    const name = `E2E CSP item ${Date.now()}`;
    const shopping = page.locator('section').filter({ has: page.getByRole('heading', { name: 'Shopping list' }) });
    await shopping.locator('#shopping-add-name').fill(name);
    await shopping.locator('#shopping-add-amount').fill('1');
    await shopping.locator('#shopping-add-unit').selectOption({ index: 0 });
    await shopping.getByRole('button', { name: 'Add to list' }).click();
    await expect(shopping.locator('li').filter({ hasText: name })).toBeVisible();
    csp.expectNone('groceries');
  });
});

test.describe('meals', () => {
  test('ingredient lines can be added and removed (Alpine recipe form)', async ({ page, csp }) => {
    await visit(page, '/meals');
    await page.locator('summary', { hasText: 'Add a recipe' }).click();

    const form = page.locator('form[action="/meals/recipes"]');
    const lines = form.locator('input[name="ingredient_name"]');
    const before = await lines.count();

    await form.getByRole('button', { name: '+ Add ingredient' }).click();
    await expect(lines).toHaveCount(before + 1);

    await form.getByRole('button', { name: 'remove' }).last().click();
    await expect(lines).toHaveCount(before);
    csp.expectNone('meals');
  });
});

test.describe('calendar', () => {
  test('the calendar renders with zero violations', async ({ page, csp }) => {
    await visit(page, '/calendar');
    await expect(page.getByRole('heading', { name: /calendar/i }).first()).toBeVisible();
    csp.expectNone('calendar');
  });
});

test.describe('photos and albums', () => {
  test('the file picker and a drop both enqueue uploads', async ({ page, csp }) => {
    await visit(page, '/photos');

    const picked = writeTempPng();
    const dropped = writeTempPng();
    try {
      await page.locator('#photo-file').setInputFiles(picked);
      await expect(page.getByTestId('upload-summary')).toHaveText(/1 uploaded/);

      // Dispatch a real drop event carrying a File, the path a desktop
      // drag-and-drop takes (handleDrop in upload-queue.js).
      const bytes = [...fs.readFileSync(dropped)];
      const dataTransfer = await page.evaluateHandle((data) => {
        const transfer = new DataTransfer();
        transfer.items.add(new File([new Uint8Array(data)], 'dropped.png', { type: 'image/png' }));
        return transfer;
      }, bytes);
      await page.getByTestId('upload-dropzone').dispatchEvent('drop', { dataTransfer });
      await expect(page.getByTestId('upload-summary')).toHaveText(/1 uploaded/);
    } finally {
      fs.rmSync(picked, { force: true });
      fs.rmSync(dropped, { force: true });
    }
    csp.expectNone('photos');
  });

  test('an album page and the viewer render', async ({ page, csp }) => {
    await visit(page, '/photos');
    const name = `E2E CSP Album ${Date.now()}`;
    await page.locator('#create-album-name').fill(name);
    await page.locator('#create-rotation').fill('5');
    const createForm = page.locator('form', { has: page.locator('#create-album-name') });
    await createForm.getByRole('button', { name: 'Create album' }).click();
    await expect(page.getByTestId('album-list').getByText(name)).toBeVisible();

    await page.getByTestId('album-list').locator('li').filter({ hasText: name })
      .getByRole('link', { name: 'View slideshow' }).click();
    await page.waitForURL(/\/album\/[^/]+$/);
    await page.waitForLoadState('networkidle');
    csp.expectNone('album');
  });
});

test.describe('public pages', () => {
  test.use({ storageState: { cookies: [], origins: [] } });

  test('login renders with zero violations', async ({ page, csp }) => {
    await visit(page, '/login');
    await expect(page.locator('input[name="email"]')).toBeVisible();
    csp.expectNone('login');
  });

  test('the offline page renders with zero violations', async ({ page, csp }) => {
    await visit(page, '/offline');
    await expect(page.getByTestId('offline-page')).toBeVisible();
    csp.expectNone('offline');
  });
});

test.describe('the watcher', () => {
  test.use({ storageState: { cookies: [], origins: [] } });

  // Negative control: without this, a watcher that silently records nothing
  // would turn every other test in the file into a pass.
  base('reports a blocked inline script and a blocked remote image', async ({ page }) => {
    const watcher = await watchCSP(page);
    await visit(page, '/login');
    await page.evaluate(() => {
      const script = document.createElement('script');
      script.textContent = 'window.__inlineRan = true';
      document.head.appendChild(script);
      const img = document.createElement('img');
      img.src = 'https://example.invalid/pixel.png';
      document.body.appendChild(img);
    });
    await expect.poll(() => watcher.violations().map((v) => v.directive)).toEqual(
      expect.arrayContaining([expect.stringMatching(/^script-src/), expect.stringMatching(/^img-src/)]),
    );
    expect(await page.evaluate(() => window.__inlineRan)).toBeUndefined();
  });
});

test.describe('settings, MFA, kiosk and passkeys', () => {
  // Two disposable members, because each test changes the member's second
  // factors: enrolling TOTP makes a passkey registration demand step-up, and a
  // registered passkey hides the TOTP enrolment form.
  const stamp = Date.now();
  const mfaMember = { email: `csp-mfa-${stamp}@test.local`, displayName: 'CSP MFA Probe' };
  const passkeyMember = { email: `csp-passkey-${stamp}@test.local`, displayName: 'CSP Passkey Probe' };
  const probes = [mfaMember, passkeyMember];

  test.beforeAll(() => {
    for (const probe of probes) {
      psql(`
        INSERT INTO identity.member
          (id, household_id, display_name, role, email, password_hash, active, created_at, updated_at)
        SELECT gen_random_uuid(), ${ownerHouseholdSQL()}, '${probe.displayName}', 'adult',
               '${probe.email}', password_hash, true, now(), now()
        FROM identity.member WHERE email = '${requireEnv('NESTOVA_EMAIL')}' LIMIT 1;
      `);
    }
  });

  test.afterAll(() => {
    for (const probe of probes) {
      const memberId = `(SELECT id FROM identity.member WHERE email = '${probe.email}')`;
      psql(`
        DELETE FROM identity.member_mfa WHERE member_id = ${memberId};
        DELETE FROM identity.member_credential WHERE member_id = ${memberId};
        DELETE FROM identity.member WHERE email = '${probe.email}';
      `);
    }
  });

  // The probes share the owner's password hash, so every account here signs
  // in with NESTOVA_PASSWORD. Login throttling (NES-86) keys on client IP and
  // counts successes, and the whole suite signs in from one loopback address,
  // so each sign-in presents its own X-Forwarded-For (loopback is a trusted
  // proxy by default) rather than spending the shared 20-per-15-minutes budget.
  async function signIn(page, email) {
    const octet = () => crypto.randomInt(1, 255);
    await page.context().setExtraHTTPHeaders({ 'X-Forwarded-For': `10.${octet()}.${octet()}.${octet()}` });
    await page.goto('/login');
    await page.fill('input[name="email"]', email);
    await page.fill('input[name="password"]', requireEnv('NESTOVA_PASSWORD'));
    await page.click('button[type="submit"]:has-text("Sign in")');
    await page.waitForURL((u) => ['/', '/login/mfa'].includes(new URL(u).pathname));
  }

  test.describe.configure({ mode: 'serial' });
  test.use({ storageState: { cookies: [], origins: [] } });

  test('registering a passkey runs the WebAuthn ceremony under the policy', async ({ page, csp }) => {
    await signIn(page, passkeyMember.email);
    await visit(page, '/settings');
    const add = page.getByRole('button', { name: 'Add a passkey' });
    test.skip((await add.count()) === 0, 'WebAuthn is not wired: start the server with PUBLIC_BASE_URL set');

    const cdp = await page.context().newCDPSession(page);
    await cdp.send('WebAuthn.enable');
    await cdp.send('WebAuthn.addVirtualAuthenticator', {
      options: {
        protocol: 'ctap2', transport: 'internal', hasResidentKey: true,
        hasUserVerification: true, isUserVerified: true, automaticPresenceSimulation: true,
      },
    });

    await add.click();
    await page.fill('#webauthn-new-nickname', 'CSP virtual key');
    await Promise.all([
      page.waitForEvent('load'),
      page.click('#webauthn-devices form button[type="submit"]'),
    ]);
    await expect(page.locator('#webauthn-devices input[name="nickname"]').first()).toBeVisible();

    csp.expectNone('passkey registration');
  });

  test('the MFA enrolment QR code loads and login then asks for the second factor', async ({ page, csp }) => {
    await signIn(page, mfaMember.email);
    await visit(page, '/settings');

    await page.locator('form[action="/settings/mfa/enroll"] button[type="submit"]').click();
    const qr = page.getByAltText('Authenticator app QR code');
    await expect(qr).toBeVisible();
    // A data: image blocked by img-src still lays out at its width/height
    // attributes, so confirm the browser actually decoded it.
    await expect.poll(() => qr.evaluate((img) => img.complete && img.naturalWidth > 0)).toBe(true);

    const secret = await page.locator('#mfa-manual-secret').inputValue();
    await page.locator('#mfa-confirm-code').fill(totp.code(secret));
    await page.locator('#mfa-confirm-code').locator('xpath=ancestor::form[1]').getByRole('button').click();
    await expect(page.getByText(/recovery code/i).first()).toBeVisible();

    // A fresh sign-in for an enrolled member lands on the second-factor page.
    await page.context().clearCookies();
    await signIn(page, mfaMember.email);
    await expect(page).toHaveURL(/\/login\/mfa(\?|$)/);
    await expect(page.locator('input[name="code"]').first()).toBeVisible();
    csp.expectNone('MFA enrolment and login MFA');
  });

  test('the kiosk activation code is generated and the kiosk views render', async ({ page, browser, csp }) => {
    await signIn(page, requireEnv('NESTOVA_EMAIL'));
    await visit(page, '/settings');
    await page.locator('#kiosk-name').fill('CSP kiosk');
    await page.getByRole('button', { name: 'Generate activation code' }).click();
    const code = await page.locator('#kiosk-code-value').inputValue();
    expect(code).toBeTruthy();

    // The kiosk is a separate device with no member session.
    const device = await browser.newContext();
    try {
      const kiosk = await device.newPage();
      await csp.watch(kiosk, 'kiosk device');
      await visit(kiosk, `/kiosk/activate?code=${encodeURIComponent(code)}`);
      await kiosk.locator('form[action="/kiosk/activate"]').getByRole('button').click();
      await kiosk.waitForURL((u) => new URL(u).pathname.startsWith('/kiosk/') && !u.toString().includes('activate'));
      for (const path of ['/kiosk/chores', '/kiosk/meals', '/kiosk/shopping', '/kiosk/calendar', '/kiosk/photos']) {
        await visit(kiosk, path);
      }
    } finally {
      await device.close();
    }
    csp.expectNone('settings');
  });
});
