// §1 setup and onboarding: the first-run wizard and the empty-database edges.
//
// Every test here needs a state the shared checklist database can never be in
// again (an unconfigured server, or no household at all), so each describe
// runs its OWN Nestova process against its OWN database — see
// helpers-onboarding.js. Nothing here touches household A, so this file runs
// green in the checklist project straight after `--project=fresh`, with no
// reset and in any order.
//
// Correction to T-1.1.1's "Run status" note: the /setup wizard DOES exist. It
// is served only by an UNCONFIGURED server (no DATABASE_URL and no state
// file — bootstrap.NeedsSetup); the checklist's shared server always has a
// DATABASE_URL, which is why /setup looked like a 404 there.
const { test, expect } = require('@playwright/test');
const { postForm, csrfToken } = require('./helpers');
const {
  PRIVATE_BASE_URL,
  PG_PORT,
  PG_USER,
  PG_PASSWORD,
  privatePsql,
  recreateDatabase,
  migrateDatabase,
  setupModeServer,
  stateFileServer,
  configuredServer,
  removeAllHouseholds,
  onboard,
} = require('./helpers-onboarding');

const PASSWORD = 'testtest1234';
const at = (p) => `${PRIVATE_BASE_URL}${p}`;

// ---------------------------------------------------------------------------
// The first-run setup wizard, on an unconfigured server.
// ---------------------------------------------------------------------------
test.describe('§1 first-run setup wizard', () => {
  test.describe.configure({ mode: 'serial', timeout: 180_000 });

  const SETUP_DB = 'nestova_private_setup';
  let wizard;

  test.beforeAll(async ({}, testInfo) => {
    testInfo.setTimeout(120_000);
    recreateDatabase(SETUP_DB);
    wizard = setupModeServer();
    await wizard.server.start('/setup');
  });

  test.afterAll(async () => {
    await wizard.server.stop();
  });

  test.afterEach(() => {
    expect(wizard.server.serverErrors(), 'the wizard server logged a 5xx or panic').toEqual([]);
  });

  // fillWizard fills the discrete-field form; overrides replace single fields.
  async function fillWizard(page, overrides = {}) {
    const fields = {
      host: '127.0.0.1',
      port: PG_PORT,
      database: SETUP_DB,
      user: PG_USER,
      password: PG_PASSWORD,
      ...overrides,
    };
    await page.locator('select[name="provider"]').selectOption('postgres');
    for (const [name, value] of Object.entries(fields)) {
      await page.locator(`input[name="${name}"]`).fill(value);
    }
    await page.locator('select[name="sslmode"]').selectOption('disable');
  }

  // submitWizard clicks Connect and returns the POST /setup response and how
  // long it took.
  async function submitWizard(page) {
    const started = Date.now();
    const [res] = await Promise.all([
      page.waitForResponse((r) => r.url().endsWith('/setup') && r.request().method() === 'POST', { timeout: 60_000 }),
      page.getByRole('button', { name: 'Connect & continue' }).click(),
    ]);
    return { status: res.status(), ms: Date.now() - started };
  }

  test('T-1.2.4 an unreachable database host errors promptly instead of hanging', async ({ page }) => {
    await page.goto(at('/setup'));
    // 192.0.2.1 is TEST-NET-1: never routed, so the connect has to time out
    // (setupConnectTimeout, 5s) rather than be refused.
    await fillWizard(page, { host: '192.0.2.1' });
    const blackhole = await submitWizard(page);
    expect(blackhole.status, 'an unreachable host must be a 422, not a 5xx or a hang').toBe(422);
    expect(blackhole.ms, 'the wizard must give up on a dead host within its connect budget').toBeLessThan(20_000);
    await expect(page.getByRole('alert')).toContainText('Could not connect to the database');

    // An unresolvable name fails at DNS, the other half of "unreachable".
    await fillWizard(page, { host: 'no-such-host.invalid' });
    const dns = await submitWizard(page);
    expect(dns.status).toBe(422);
    expect(dns.ms).toBeLessThan(20_000);
    await expect(page.getByRole('alert')).toContainText('Could not connect to the database');
  });

  test('T-1.2.3 bad database credentials give a readable error and the wizard stays usable (NES-164)', async ({ page }) => {
    await page.goto(at('/setup'));
    await fillWizard(page, { password: 'definitely-not-the-password' });
    const first = await submitWizard(page);
    expect(first.status).toBe(422);

    // NES-164 served an EMPTY 200 here; the form itself must re-render.
    await expect(page.getByRole('heading', { name: 'Welcome to Nestova' })).toBeVisible();
    await expect(page.getByRole('alert')).toHaveText(
      'Could not connect to the database with those details. Check the host, port, and credentials.',
    );
    // Sticky fields, but the password is never echoed back into the page.
    await expect(page.locator('input[name="host"]')).toHaveValue('127.0.0.1');
    await expect(page.locator('input[name="database"]')).toHaveValue(SETUP_DB);
    await expect(page.locator('input[name="password"]')).toHaveValue('');
    expect(await page.content()).not.toContain('definitely-not-the-password');

    // Still usable: the re-rendered form (and its CSRF token) accepts a second
    // submission and answers it properly, rather than a 403 or a blank page.
    await fillWizard(page, { user: 'no_such_role' });
    const second = await submitWizard(page);
    expect(second.status).toBe(422);
    await expect(page.getByRole('alert')).toContainText('Could not connect to the database');
  });

  test('T-1.3.1 mid-setup, app routes redirect to the wizard and render no app shell', async ({ page }) => {
    for (const path of ['/tasks', '/', '/settings', '/onboarding']) {
      await page.goto(at(path));
      expect(new URL(page.url()).pathname, `${path} while unconfigured`).toBe('/setup');
      await expect(page.getByRole('heading', { name: 'Welcome to Nestova' })).toBeVisible();
      await expect(page.locator('nav[aria-label="Primary"]'), `${path} leaked the app shell`).toHaveCount(0);
    }
  });

  test('T-1.1.2 a valid database config completes setup and proceeds to /onboarding', async ({ page }) => {
    await page.goto(at('/setup'));
    await fillWizard(page);
    const done = await submitWizard(page);
    expect(done.status).toBe(200);
    await expect(page.getByRole('heading', { name: "You're all set" })).toBeVisible();

    // The completion page meta-refreshes to /onboarding once the process has
    // restarted in normal mode against the database it just migrated.
    await page.waitForURL((u) => new URL(u).pathname === '/onboarding', { timeout: 120_000 });
    await expect(page.locator('input[name="household_name"]')).toBeVisible({ timeout: 60_000 });
    expect(privatePsql(SETUP_DB, 'SELECT count(*) FROM identity.household;').trim()).toBe('0');
  });

  test('T-1.2.1 /setup is not reachable once setup is complete, even after a restart', async ({ page }) => {
    const res = await page.goto(at('/setup'));
    expect(res.status(), '/setup after setup').toBe(404);
    await expect(page.locator('select[name="provider"]')).toHaveCount(0);

    // A POST cannot re-run the wizard (and so re-point the database) either.
    const post = await page.request.post(at('/setup'), {
      form: { host: '127.0.0.1', port: PG_PORT, database: SETUP_DB, user: PG_USER, password: PG_PASSWORD },
      maxRedirects: 0,
    });
    expect([404, 405], 'POST /setup after setup').toContain(post.status());

    // A reboot reads the persisted state file and boots normally, without the
    // force flag the first boot needed.
    await wizard.server.stop();
    wizard.server = stateFileServer(wizard.stateFile);
    await wizard.server.start('/onboarding');
    const afterRestart = await page.goto(at('/setup'));
    expect(afterRestart.status(), '/setup after a restart').toBe(404);
  });
});

// ---------------------------------------------------------------------------
// Onboarding against a migrated database with no household.
// ---------------------------------------------------------------------------
test.describe('§1 onboarding on an empty database', () => {
  test.describe.configure({ timeout: 90_000 });

  const DB = 'nestova_private';
  let server;
  let errorsBefore = 0;

  test.beforeAll(async ({}, testInfo) => {
    testInfo.setTimeout(120_000);
    recreateDatabase(DB);
    migrateDatabase(DB);
    server = configuredServer(DB);
    await server.start('/onboarding');
  });

  test.afterAll(async () => {
    await server.stop();
  });

  test.beforeEach(() => {
    removeAllHouseholds(DB);
    errorsBefore = server.serverErrors().length;
  });

  test.afterEach(() => {
    expect(server.serverErrors().slice(errorsBefore), 'the server logged a 5xx or panic').toEqual([]);
  });

  const count = (table) => Number(privatePsql(DB, `SELECT count(*) FROM ${table};`).trim());

  function onboardingFields(overrides = {}) {
    const suffix = `${Date.now()}${Math.floor(Math.random() * 1000)}`;
    return {
      household_name: `Private Household ${suffix}`,
      display_name: `Founder ${suffix}`,
      email: `founder-${suffix}@test.local`,
      password: PASSWORD,
      ...overrides,
    };
  }

  test('T-1.3.4 the first member cannot be created as a child — onboarding always makes an owner', async ({ page }) => {
    await page.goto(at('/onboarding'));
    await expect(page.locator('select[name="role"], input[name="role"]'), 'onboarding offers no role choice').toHaveCount(0);

    // Forge the field the form does not offer. This is also the describe's
    // sanity guard: a complete, valid onboarding payload is accepted (303).
    const csrf_token = await csrfToken(page, at('/onboarding'));
    const status = await postForm(page, '/onboarding', { csrf_token, ...onboardingFields(), role: 'child' });
    expect(status, 'a valid onboarding payload must be accepted').toBe(303);

    expect(privatePsql(DB, 'SELECT role FROM identity.member;').trim()).toBe('owner');
    expect(count('identity.household')).toBe(1);
  });

  test('T-1.3.2 two browsers onboarding concurrently create exactly one household', async ({ browser }) => {
    for (let round = 1; round <= 3; round += 1) {
      removeAllHouseholds(DB);
      const contexts = [await browser.newContext(), await browser.newContext()];
      const pages = await Promise.all(contexts.map((c) => c.newPage()));
      const tokens = [];
      for (const p of pages) tokens.push(await csrfToken(p, at('/onboarding')));

      // No await between the two: both POSTs are in flight together.
      const statuses = await Promise.all(pages.map((p, i) =>
        postForm(p, '/onboarding', { csrf_token: tokens[i], ...onboardingFields({ household_name: `Race ${round}-${i}` }) })));

      expect(statuses, `round ${round}: winner goes to /, loser to /login — both 303`).toEqual([303, 303]);
      expect(count('identity.household'), `round ${round}: households`).toBe(1);
      expect(count('identity.member'), `round ${round}: members`).toBe(1);

      // Exactly one browser is signed in: the loser was sent to log in.
      const landed = [];
      for (const p of pages) {
        await p.goto(at('/'));
        landed.push(new URL(p.url()).pathname);
      }
      expect(landed.sort(), `round ${round}: one dashboard, one login`).toEqual(['/', '/login']);
      await Promise.all(contexts.map((c) => c.close()));
    }
  });

  test('T-1.3.3 refresh and back mid-wizard leave no duplicate household or orphan member', async ({ page }) => {
    const fields = onboardingFields();

    // A failed submit, then a refresh of that POST result: nothing persisted.
    await page.goto(at('/onboarding'));
    const staleToken = await page.locator('input[name="csrf_token"]').inputValue();
    await page.fill('input[name="household_name"]', fields.household_name);
    await page.fill('input[name="display_name"]', fields.display_name);
    await page.fill('input[name="email"]', fields.email);
    await page.fill('input[name="password"]', 'short');
    await page.click('button:has-text("Create household")');
    await expect(page.getByRole('alert')).toContainText('at least 8 characters');
    await page.reload();
    expect(count('identity.household'), 'after refreshing a failed submit').toBe(0);
    expect(count('identity.member'), 'after refreshing a failed submit').toBe(0);

    // The real submit.
    await page.goto(at('/onboarding'));
    await page.fill('input[name="household_name"]', fields.household_name);
    await page.fill('input[name="display_name"]', fields.display_name);
    await page.fill('input[name="email"]', fields.email);
    await page.fill('input[name="password"]', PASSWORD);
    await page.click('button:has-text("Create household")');
    await page.waitForURL((u) => new URL(u).pathname === '/', { timeout: 15_000 });

    // Back to the wizard: it no longer renders; setup is done.
    await page.goBack();
    await page.reload();
    expect(new URL(page.url()).pathname, 'back + refresh onto /onboarding').toBe('/login');

    // Resubmitting the original form (a browser "confirm resubmission") is
    // turned away rather than creating a second household or member.
    const replay = await postForm(page, '/onboarding', { csrf_token: staleToken, ...fields });
    expect([303, 403], 'a replayed onboarding POST').toContain(replay);

    expect(count('identity.household'), 'households').toBe(1);
    expect(count('identity.member'), 'members').toBe(1);
    expect(Number(privatePsql(DB, `
      SELECT count(*) FROM identity.member m
       WHERE NOT EXISTS (SELECT 1 FROM identity.household h WHERE h.id = m.household_id)
          OR m.email IS NULL;`).trim()), 'orphaned or credential-less members').toBe(0);
  });

  test('T-1.3.5 [!] a 10,000-character household name is refused', async ({ page }) => {
    test.fail(true, 'DEFECT: onboarding stores a 10,000-character household name (no length cap in handler, domain or schema)');
    const csrf_token = await csrfToken(page, at('/onboarding'));
    const status = await postForm(page, '/onboarding', {
      csrf_token, ...onboardingFields({ household_name: 'H'.repeat(10_000) }),
    });
    const stored = count('identity.household');
    expect({ status, stored }, 'an unbounded household name must be refused, not stored')
      .toEqual({ status: 422, stored: 0 });
  });

  test('onboarding stage: /tasks with no household redirects to login, with no app shell', async ({ page }) => {
    await page.goto(at('/tasks'));
    const landed = new URL(page.url());
    expect(landed.pathname).toBe('/login');
    await expect(page.locator('nav[aria-label="Primary"]')).toHaveCount(0);
  });

  // ---- Member provisioning edges: owner signed in on a fresh household ----

  async function signInFreshOwner(page) {
    const fields = onboardingFields();
    await onboard(page, {
      householdName: fields.household_name,
      displayName: fields.display_name,
      email: fields.email,
      password: PASSWORD,
    });
    return fields;
  }

  async function addMember(page, fields) {
    const csrf_token = await csrfToken(page, at('/members/new'));
    return postForm(page, '/members', { csrf_token, role: 'adult', ...fields });
  }

  // addMemberPage submits through the real form and returns the alert text.
  async function addMemberViaForm(page, { displayName, email }) {
    await page.goto(at('/members/new'));
    await page.fill('input[name="display_name"]', displayName);
    await page.fill('input[name="email"]', email);
    await page.fill('input[name="password"]', PASSWORD);
    const [res] = await Promise.all([
      page.waitForResponse((r) => r.url().endsWith('/members') && r.request().method() === 'POST'),
      page.click('button:has-text("Add member")'),
    ]);
    return res.status();
  }

  test('T-1.2.5 a duplicate member display name is refused (ErrDuplicateMember)', async ({ page }) => {
    const owner = await signInFreshOwner(page);
    const name = `Jamie ${Date.now()}`;
    expect(await addMember(page, { display_name: name }), 'first member with the name').toBe(303);

    for (const dup of [name, name.toUpperCase(), `  ${name}  `, owner.display_name]) {
      const status = await addMemberViaForm(page, { displayName: dup, email: `dup-${Date.now()}@test.local` });
      expect(status, `duplicate "${dup}"`).toBe(409);
      await expect(page.getByRole('alert')).toHaveText('A member with that name already exists in your household.');
    }
    expect(count('identity.member'), 'owner + the one accepted member').toBe(2);
  });

  test('T-1.2.6 a duplicate email is refused (ErrEmailAlreadyInUse)', async ({ page }) => {
    const owner = await signInFreshOwner(page);
    const email = `unique-${Date.now()}@test.local`;
    expect(await addMember(page, { display_name: `First ${Date.now()}`, email, password: PASSWORD }), 'first use of the email').toBe(303);

    const dups = [email, email.toUpperCase(), owner.email];
    for (const [i, dup] of dups.entries()) {
      const status = await addMemberViaForm(page, { displayName: `Second ${i} ${Date.now()}`, email: dup });
      expect(status, `duplicate email "${dup}"`).toBe(409);
      await expect(page.getByRole('alert')).toHaveText('That email address is already in use by another member.');
    }
    expect(count('identity.member'), 'owner + the one accepted member').toBe(2);
  });

  test('email formats a real address can take are accepted, and the member can sign in', async ({ page, browser }) => {
    // Sanity guard for T-1.2.7: a dotless domain is valid per the HTML email
    // grammar, and an IDN domain is a real address. Both must be accepted,
    // so the refusals asserted in T-1.2.7 are about the format, not the route.
    // Added through the real form: Chromium punycode-encodes an IDN typed
    // into an <input type=email>, so form-added and form-typed logins agree.
    await signInFreshOwner(page);
    const stamp = Date.now();
    const accepted = [`dotless${stamp}@b`, `idn${stamp}@bücher.example`];
    for (const [i, email] of accepted.entries()) {
      const status = await addMemberViaForm(page, { displayName: `Format ok ${i} ${stamp}`, email });
      expect(status, email).toBe(303);
    }

    const ctx = await browser.newContext();
    const other = await ctx.newPage();
    await other.goto(at('/login'));
    await other.fill('input[name="email"]', accepted[1]);
    await other.fill('input[name="password"]', PASSWORD);
    await other.click('button:has-text("Sign in")');
    await other.waitForURL((u) => new URL(u).pathname === '/', { timeout: 15_000 });
    await ctx.close();
  });

  test('T-1.2.7 malformed emails are refused: a@@b.com and a 300-character local part', async ({ page }) => {
    test.fail(true, 'DEFECT: email validation is only strings.Contains(email, "@"), so a@@b.com and a 300-char local part are stored');
    await signInFreshOwner(page);
    const stamp = Date.now();
    const malformed = {
      doubleAt: `a${stamp}@@b.com`,
      longLocal: `${'l'.repeat(300)}${stamp}@test.local`,
    };
    const results = {};
    for (const [key, email] of Object.entries(malformed)) {
      results[key] = await addMember(page, { display_name: `Format bad ${key} ${stamp}`, email, password: PASSWORD });
    }
    // Onboarding uses the same check; prove it on the first-run form too.
    removeAllHouseholds(DB);
    const csrf_token = await csrfToken(page, at('/onboarding'));
    results.onboardingDoubleAt = await postForm(page, '/onboarding', {
      csrf_token, ...onboardingFields({ email: `o${stamp}@@b.com` }),
    });
    expect(results, 'every malformed email must be refused with 422').toEqual({
      doubleAt: 422, longLocal: 422, onboardingDoubleAt: 422,
    });
  });

  test('T-1.2.8 birthday validation (future, 1800, 2026-02-30, empty)', async () => {
    test.skip(true, 'Not built: no member form, domain type or column carries a birthday '
      + '(no match for birthday/birth_date/dob in web/, internal/ or the migrations)');
  });
});
