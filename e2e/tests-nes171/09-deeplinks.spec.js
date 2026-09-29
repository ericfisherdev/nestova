// §13 QR deep links — zero automated coverage before this run.
//
// A real link only ever appears inside a QR PNG on a kiosk screen, so the
// tests mint their own using the same derivation the server uses:
//   key = HMAC-SHA256(SESSION_SECRET, "nestova:deeplink:v1")
//   sig = base64url-raw(HMAC-SHA256(key, path + "|" + exp))
// That is a deliberate duplication of production logic. It buys the ability to
// test expiry and tampering directly, and if the server's scheme ever changes
// the VALID-link test below fails first, which is the signal to update this.
//
// Tests that act on a link (claim, redeem, log in through it) each seed their
// own member, so the confirm POST's per-member rate limit and any state the
// action leaves behind never leak between tests or into the shared personas.
const { test, expect } = require('@playwright/test');
const crypto = require('crypto');
const { PASSWORD, PERSONAS } = require('../tests/fixtures');
const { login } = require('./helpers');
const { psql, seedHouseholdB, seedMemberInA } = require('../tests/db');
const { randomInt } = require('node:crypto');

const SECRET = process.env.NESTOVA_SESSION_SECRET || 'dev-only-insecure-session-secret-change-me';
const PURPOSE = 'nestova:deeplink:v1';
const RESCAN_HEADING = "This link isn't valid anymore";

function signerKey(secret = SECRET) {
  return crypto.createHmac('sha256', secret).update(PURPOSE).digest();
}

function sign(path, expiryUnix, secret = SECRET) {
  const mac = crypto.createHmac('sha256', signerKey(secret)).update(`${path}|${expiryUnix}`).digest();
  return mac.toString('base64url');
}

function signedURL(path, { secondsFromNow = 300, secret = SECRET } = {}) {
  const exp = Math.floor(Date.now() / 1000) + secondsFromNow;
  return `${path}?exp=${exp}&sig=${encodeURIComponent(sign(path, exp, secret))}`;
}

function householdId(name) {
  return psql(`SELECT id FROM identity.household WHERE name = '${name}' LIMIT 1;`).trim();
}

function uniqueSuffix() {
  return `${Date.now()}-${randomInt(1_000_000)}`;
}

// seedPersona adds a fresh member to household A and returns its id plus a
// persona that can sign in with the shared fixture password.
function seedPersona(label, role = 'adult') {
  const suffix = uniqueSuffix();
  const displayName = `Deeplink ${label} ${suffix}`;
  const email = `deeplink-${label.toLowerCase()}-${suffix}@test.local`;
  const id = seedMemberInA({ displayName, email, role, copyHashFrom: PERSONAS.owner.email });
  return { id, persona: { email, password: PASSWORD, displayName, role } };
}

// seedInstance creates a claimable chore and its pending instance in the
// named household. assignee null leaves it open to claim, which is the only
// state a claim-task link can act on.
function seedInstance({ household = householdId('Household A'), assignee = null, title = 'Deep link probe' } = {}) {
  const taskId = psql(`
    INSERT INTO nestova.recurring_task
      (id, household_id, title, category, cadence, rotation_policy, points, lead_time_days, active, created_at, updated_at)
    VALUES (gen_random_uuid(), '${household}', '${title}', 'chore',
            '{"Freq":"daily","Interval":1,"Anchor":"2026-08-25T00:00:00Z","ByWeekday":null}'::jsonb,
            'claimable', 5, 0, true, now(), now())
    RETURNING id;`).trim();
  const assigneeSQL = assignee ? `'${assignee}'` : 'NULL';
  return psql(`
    INSERT INTO nestova.task_instance
      (id, recurring_task_id, household_id, assignee_id, due_on, status, kind, created_at, updated_at)
    VALUES (gen_random_uuid(), '${taskId}', '${household}', ${assigneeSQL}, current_date, 'pending', 'scheduled', now(), now())
    RETURNING id;`).trim();
}

function claimedBy(instanceId) {
  return psql(`SELECT coalesce(claimed_by::text, '') FROM nestova.task_instance WHERE id = '${instanceId}';`).trim();
}

function grantPoints(member, points) {
  psql(`
    INSERT INTO nestova.point_ledger (id, household_id, member_id, source_type, source_id, points, created_at)
    VALUES (gen_random_uuid(), '${householdId('Household A')}', '${member}', 'adjustment', gen_random_uuid(), ${points}, now());
  `);
}

function balance(member) {
  return Number(psql(
    `SELECT coalesce(sum(points), 0) FROM nestova.point_ledger WHERE member_id = '${member}';`,
  ).trim());
}

function seedReward({ cost, quantity }) {
  return psql(`
    INSERT INTO nestova.reward
      (id, household_id, name, cost_points, active, quantity_available, created_at, updated_at)
    VALUES (gen_random_uuid(), '${householdId('Household A')}', 'Deep link reward ${uniqueSuffix()}',
            ${cost}, true, ${quantity}, now(), now())
    RETURNING id;
  `).trim();
}

function redemptionCount(member, reward) {
  return Number(psql(
    `SELECT count(*) FROM nestova.reward_redemption WHERE member_id = '${member}' AND reward_id = '${reward}';`,
  ).trim());
}

async function statusOf(page, path) {
  const res = await page.request.get(path, { maxRedirects: 0 });
  return res.status();
}

// confirmPOST submits the confirm form for a signed link from inside the
// page's session and returns the status, mapping a manual redirect to 303.
async function confirmPOST(page, link, fields = {}) {
  const csrf = await page.locator('input[name="csrf_token"]').first().inputValue();
  return page.evaluate(async ({ link, body }) => {
    const res = await fetch(link, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body,
      redirect: 'manual',
    });
    return res.type === 'opaqueredirect' ? 303 : res.status;
  }, { link, body: new URLSearchParams({ csrf_token: csrf, ...fields }).toString() });
}

test.describe('§13.1 deep link happy paths', () => {
  test('T-13.1.1 a valid link opens its action screen, performs the action and lands on done', async ({ page }) => {
    const id = seedInstance();
    const { id: memberId, persona } = seedPersona('Claim');
    await login(page, persona);

    const link = signedURL(`/go/claim-task/${id}`);
    const res = await page.goto(link);
    expect(res.status(), 'a validly signed link must be accepted').toBe(200);
    await expect(page.getByRole('heading', { name: 'Claim this chore?' })).toBeVisible();
    await expect(page.getByTestId('deeplink-confirm')).toContainText('Deep link probe');
    expect(claimedBy(id), 'opening the link alone must not act').toBe('');

    await page.getByRole('button', { name: 'Claim' }).click();
    await page.waitForURL((u) => new URL(u).pathname === '/go/claim-task/done');
    await expect(page.getByRole('heading', { name: 'Chore claimed' })).toBeVisible();
    expect(claimedBy(id), 'the confirm must claim the chore for the scanning member').toBe(memberId);
  });

  test('T-13.1.2 the quick add-chore link leads to a chore being created', async ({ page }) => {
    const { id: memberId, persona } = seedPersona('AddChore');
    await login(page, persona);
    const title = `Deep link chore ${uniqueSuffix()}`;

    await page.goto(signedURL('/go/add-chore'));
    await expect(page.getByRole('heading', { name: 'Add a chore' })).toBeVisible();
    await page.getByRole('button', { name: 'Continue' }).click();
    await page.waitForURL((u) => new URL(u).pathname === '/tasks/new');

    await page.fill('input[name="title"]', title);
    await page.fill('input[name="interval"]', '1');
    await page.selectOption('select[name="rotation_policy"]', 'fixed');
    await page.locator(`input[name="pool"][value="${memberId}"]`).check();
    await page.getByRole('button', { name: 'Save chore' }).click();
    await page.waitForURL((u) => new URL(u).pathname === '/tasks');

    expect(
      psql(`SELECT count(*) FROM nestova.recurring_task WHERE title = '${title}';`).trim(),
      'the chore reached through the deep link must be stored',
    ).toBe('1');
  });

  test('T-13.1.3 a scan while signed out goes through login and lands on the intended action', async ({ browser }) => {
    const id = seedInstance();
    const { id: memberId, persona } = seedPersona('Scan');
    const link = signedURL(`/go/claim-task/${id}`);

    const anon = await browser.newContext();
    const page = await anon.newPage();
    try {
      // A signed-but-unauthenticated scan hands off to login (303) rather than
      // dead-ending with a 401.
      expect([303, 302]).toContain(await statusOf(page, link));

      await page.goto(link);
      await page.waitForURL((u) => new URL(u).pathname === '/login');
      await page.fill('input[name="email"]', persona.email);
      await page.fill('input[name="password"]', persona.password);
      await page.click('button:has-text("Sign in")');

      // Login continues to the SAME signed link, exp and sig intact.
      await page.waitForURL((u) => new URL(u).pathname === `/go/claim-task/${id}`);
      const landed = new URL(page.url());
      expect(`${landed.pathname}${landed.search}`).toBe(link);
      await expect(page.getByRole('heading', { name: 'Claim this chore?' })).toBeVisible();

      await page.getByRole('button', { name: 'Claim' }).click();
      await page.waitForURL((u) => new URL(u).pathname === '/go/claim-task/done');
      await expect(page.getByRole('heading', { name: 'Chore claimed' })).toBeVisible();
      expect(claimedBy(id)).toBe(memberId);
    } finally {
      await anon.close();
    }
  });
});

test.describe('§13.2 signature and target checks', () => {
  test('T-13.2.2 a tampered signature is refused', async ({ page }) => {
    const id = seedInstance();
    await login(page, PERSONAS.owner);
    const good = signedURL(`/go/claim-task/${id}`);
    const tampered = good.replace(/sig=([^&]+)/, (m, sig) => `sig=${sig.slice(0, -3)}AAA`);
    const status = await statusOf(page, tampered);
    expect(status, 'a tampered signature must be refused').toBeGreaterThanOrEqual(400);
  });

  test('T-13.2.3 swapping the id under a valid signature is refused', async ({ page }) => {
    const id = seedInstance();
    const other = seedInstance();
    await login(page, PERSONAS.owner);

    // Sign one id, then present another: the signature covers the path.
    const exp = Math.floor(Date.now() / 1000) + 300;
    const sig = sign(`/go/claim-task/${id}`, exp);
    const swapped = `/go/claim-task/${other}?exp=${exp}&sig=${encodeURIComponent(sig)}`;
    expect(await statusOf(page, swapped), 'a signature must not transfer to another id').toBeGreaterThanOrEqual(400);
  });

  test('T-13.2.1 an expired link is refused', async ({ page }) => {
    const id = seedInstance();
    await login(page, PERSONAS.owner);
    const expired = signedURL(`/go/claim-task/${id}`, { secondsFromNow: -60 });
    expect(await statusOf(page, expired), 'an expired link must be refused').toBeGreaterThanOrEqual(400);
  });

  test('T-13.2.1b extending the expiry without re-signing is refused', async ({ page }) => {
    const id = seedInstance();
    await login(page, PERSONAS.owner);
    const path = `/go/claim-task/${id}`;
    const exp = Math.floor(Date.now() / 1000) - 60;
    const sig = sign(path, exp);
    // Same signature, a far-future exp: the MAC covers the expiry too.
    const forged = `${path}?exp=${Math.floor(Date.now() / 1000) + 9999}&sig=${encodeURIComponent(sig)}`;
    expect(await statusOf(page, forged), 'the expiry must be covered by the signature').toBeGreaterThanOrEqual(400);
  });

  test('T-13.2.4 an unknown action is refused', async ({ page }) => {
    const id = seedInstance();
    await login(page, PERSONAS.owner);
    expect(await statusOf(page, signedURL(`/go/detonate/${id}`))).toBeGreaterThanOrEqual(400);
  });

  test('T-13.6 an unsigned link is refused', async ({ page }) => {
    const id = seedInstance();
    await login(page, PERSONAS.owner);
    expect(await statusOf(page, `/go/claim-task/${id}`), 'a link with no signature must be refused').toBeGreaterThanOrEqual(400);
  });

  test('T-13.2.5 an action/id mismatch is refused as not found, whatever the signature', async ({ page }) => {
    const id = seedInstance();
    await login(page, PERSONAS.owner);

    // Sanity: the same two actions in their correct shapes are accepted.
    expect(await statusOf(page, signedURL('/go/add-chore')), 'add-chore without an id is valid').toBe(200);
    expect(await statusOf(page, signedURL(`/go/claim-task/${id}`)), 'claim-task with an id is valid').toBe(200);

    // add-chore takes no id: Action.Path returns ErrMissingID, mapped to 404
    // BEFORE the signature is even checked — so a correctly signed mismatched
    // path is refused just the same.
    expect(await statusOf(page, signedURL(`/go/add-chore/${id}`)), 'add-chore must not accept an id').toBe(404);
    // claim-task requires one: an id-less path has no route at all.
    expect(await statusOf(page, signedURL('/go/claim-task')), 'claim-task must require an id').toBe(404);
    expect(claimedBy(id)).toBe('');
  });

  test('T-13.2.6 a link to household B\'s chore is refused for a household A member', async ({ page }) => {
    const { householdId: householdB } = seedHouseholdB({
      householdName: 'Household B',
      ownerName: PERSONAS.otherOwner.displayName,
      ownerEmail: PERSONAS.otherOwner.email,
      copyHashFrom: PERSONAS.owner.email,
    });
    const foreign = seedInstance({ household: householdB, title: 'Household B chore' });
    const own = seedInstance();
    const { persona } = seedPersona('Tenant');
    await login(page, persona);

    // Sanity: the member's own household's link opens.
    await page.goto(signedURL(`/go/claim-task/${own}`));
    await expect(page.getByRole('heading', { name: 'Claim this chore?' })).toBeVisible();

    // A validly signed link is not an authorization grant: B's instance is
    // looked up under A's household id and is simply not found.
    const link = signedURL(`/go/claim-task/${foreign}`);
    const res = await page.goto(link);
    expect(res.status(), 'another household\'s chore must not be shown').toBe(404);
    await expect(page.locator('body')).not.toContainText('Household B chore');

    // The confirm POST is refused the same way, and B's chore stays untouched.
    await page.goto(signedURL(`/go/claim-task/${own}`));
    expect(await confirmPOST(page, link), 'another household\'s chore must not be claimable').toBe(404);
    expect(claimedBy(foreign)).toBe('');
  });

  test('T-13.2.7 a used one-shot redeem link cannot be replayed, even concurrently', async ({ page }) => {
    const { id: kid, persona } = seedPersona('Redeem', 'child');
    grantPoints(kid, 100);
    const reward = seedReward({ cost: 10, quantity: 10 });
    const link = signedURL(`/go/redeem-reward/${reward}`);

    await login(page, persona);
    await page.goto(link);
    await expect(page.getByRole('heading', { name: 'Redeem this reward?' })).toBeVisible();

    // Two submissions of the same link at once: exactly one may redeem.
    const csrf = await page.locator('input[name="csrf_token"]').first().inputValue();
    const statuses = await page.evaluate(async ({ link, body }) => {
      const once = () => fetch(link, {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body,
        redirect: 'manual',
      }).then((r) => (r.type === 'opaqueredirect' ? 303 : r.status));
      return Promise.all([once(), once()]);
    }, { link, body: new URLSearchParams({ csrf_token: csrf }).toString() });
    expect(statuses.sort(), 'one redemption and one ErrDeepLinkAlreadyRedeemed').toEqual([303, 409]);

    // A later replay through the UI shows the friendly already-used message.
    await page.goto(link);
    await page.getByRole('button', { name: 'Redeem' }).click();
    await expect(page.getByRole('heading', { name: 'This code has already been used' })).toBeVisible();

    expect(redemptionCount(kid, reward), 'one link, one redemption').toBe(1);
    expect(balance(kid), 'points are debited once').toBe(90);

    // Sanity: a FRESH link for the same reward still redeems — the refusal is
    // about the spent signature, not the reward or the member.
    await page.goto(signedURL(`/go/redeem-reward/${reward}`, { secondsFromNow: 301 }));
    await page.getByRole('button', { name: 'Redeem' }).click();
    await page.waitForURL((u) => new URL(u).pathname === '/go/redeem-reward/done');
    expect(redemptionCount(kid, reward)).toBe(2);
  });
});

test.describe('§13.3 key rotation and login continuation', () => {
  test('T-13.3.1 a link signed with a rotated-out key is refused', async ({ page }) => {
    const id = seedInstance();
    await login(page, PERSONAS.owner);
    const path = `/go/claim-task/${id}`;

    // The signer key is derived from SESSION_SECRET and there is no key ring,
    // so rotating the secret leaves every link minted under the old one
    // unverifiable. A link signed under a different ("old") secret is exactly
    // what the server sees after a rotation.
    const stale = signedURL(path, { secret: 'previous-session-secret-before-rotation' });
    const res = await page.goto(stale);
    expect(res.status(), 'a link signed with an old key must be refused').toBe(400);
    await expect(page.getByRole('heading', { name: RESCAN_HEADING })).toBeVisible();

    // Sanity: the same path signed with the current key is accepted.
    expect(await statusOf(page, signedURL(path))).toBe(200);
  });

  test('T-13.3.2 the login continuation cannot redirect off-origin', async ({ browser, baseURL }) => {
    const origin = new URL(baseURL).origin;
    const { persona } = seedPersona('Redirect');

    // postLogin signs in from a fresh context with the given next value and
    // returns where the server redirects.
    async function postLogin(next) {
      const ctx = await browser.newContext();
      const page = await ctx.newPage();
      try {
        await page.goto(`/login?next=${encodeURIComponent(next)}`);
        const csrf = await page.locator('input[name="csrf_token"]').first().inputValue();
        const res = await page.request.post('/login', {
          form: { csrf_token: csrf, email: persona.email, password: persona.password, next },
          maxRedirects: 0,
        });
        expect(res.status(), `login with next=${next}`).toBe(303);
        return res.headers().location;
      } finally {
        await ctx.close();
      }
    }

    // Sanity: a same-origin continuation, query included, is honoured.
    expect(await postLogin('/tasks?from=qr')).toBe('/tasks?from=qr');

    const hostile = [
      'https://evil.example/steal',
      '//evil.example/steal',
      '/\\evil.example/steal',
      '/%5Cevil.example/steal',
      '/foo/..//evil.example/steal',
      'javascript:alert(1)',
      ' //evil.example',
    ];
    const escaped = [];
    for (const next of hostile) {
      const location = await postLogin(next);
      const resolved = new URL(location, origin);
      // A same-origin path that merely CONTAINS the host name (the traversal
      // case collapses to "/evil.example/steal") is harmless; leaving the
      // origin, or a protocol-relative "//" or "/\\" prefix, is not.
      if (resolved.origin !== origin || /^\/[/\\]/.test(location)) {
        escaped.push(`${next} -> ${location}`);
      }
    }
    expect(escaped, 'continuations that left the origin').toEqual([]);

    // And end to end through the real form: the browser stays on the app.
    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    try {
      await ctx.route('**://evil.example/**', (route) => route.abort());
      await page.goto(`/login?next=${encodeURIComponent('//evil.example/steal')}`);
      await page.fill('input[name="email"]', persona.email);
      await page.fill('input[name="password"]', persona.password);
      await page.click('button:has-text("Sign in")');
      await page.waitForURL((u) => new URL(u).pathname === '/');
      expect(new URL(page.url()).origin).toBe(origin);
    } finally {
      await ctx.close();
    }
  });

  test('T-13.3.3 a continuation to a route the member lacks the role for is refused after login', async ({ browser }) => {
    const target = '/admin/rewards/new';

    // continueAs scans the parent-only route signed out, signs in as persona
    // through the real form, and returns the status of the page it lands on.
    async function continueAs(persona) {
      const ctx = await browser.newContext();
      const page = await ctx.newPage();
      try {
        await page.goto(target);
        await page.waitForURL((u) => new URL(u).pathname === '/login');
        await page.fill('input[name="email"]', persona.email);
        await page.fill('input[name="password"]', persona.password);
        const [landing] = await Promise.all([
          page.waitForResponse((r) => new URL(r.url()).pathname === target),
          page.click('button:has-text("Sign in")'),
        ]);
        return landing.status();
      } finally {
        await ctx.close();
      }
    }

    // Sanity: a parent continues straight to it.
    const { persona: parent } = seedPersona('Parent', 'adult');
    expect(await continueAs(parent), 'a parent lands on the page').toBe(200);

    // A child is carried to the same URL — logging in never widens access —
    // and the route's own role gate refuses it.
    const { persona: child } = seedPersona('Child', 'child');
    expect(await continueAs(child), 'a child must be refused after login').toBe(403);
  });
});
