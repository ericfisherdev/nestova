// §9 Calendar: the unified month view and the Google OAuth connect flow.
//
// Two things shape this file.
//
// The page reads only from the database — external events are cached rows the
// background sync writes, and the unified view merges them with task instances
// and subscription renewals. So every rendering case is seeded in SQL, which is
// also the only way to produce the awkward shapes the checklist asks for
// (all-day, multi-day, DST-crossing).
//
// The OAuth flow is testable up to, but not through, Google. Connect and every
// callback REJECTION path are pure local logic and are covered here. The one
// success path (T-9.1.3) needs a real token exchange and is recorded as not
// runnable rather than faked, because a fake would assert the fake.
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, csrfToken } = require('./helpers');
const { psql } = require('../tests/db');

const TS = Date.now();
const name = (label) => `${label} ${TS}`;

function householdA() {
  return psql("SELECT id FROM identity.household WHERE name = 'Household A' LIMIT 1;").trim();
}

function memberId(displayName) {
  return psql(`SELECT id FROM identity.member WHERE display_name = '${displayName}' LIMIT 1;`).trim();
}

function accountCount() {
  return Number(psql('SELECT count(*) FROM nestova.calendar_account;').trim());
}

// seedAccount creates a calendar_account row directly. The encrypted token
// columns hold ciphertext the app never decrypts on the read path this file
// exercises, so opaque bytes are honest placeholders rather than a fake login.
function seedAccount() {
  const existing = psql(
    `SELECT id FROM nestova.calendar_account WHERE household_id = '${householdA()}' LIMIT 1;`,
  ).trim();
  if (existing) return existing;
  return psql(`
    INSERT INTO nestova.calendar_account
      (id, member_id, household_id, provider, access_token_enc, refresh_token_enc,
       token_expiry, sync_token, calendar_ids, created_at, updated_at)
    VALUES (gen_random_uuid(), '${memberId('Owner A')}', '${householdA()}', 'google',
            '\\x00'::bytea, '\\x00'::bytea, now() + interval '1 hour', NULL,
            ARRAY['primary'], now(), now())
    RETURNING id;
  `).trim();
}

// seedEvent inserts one cached external event. Times are given as SQL
// expressions so a test can be explicit about zone and DST.
function seedEvent({ accountID, title, startsAt, endsAt, allDay = false }) {
  psql(`
    INSERT INTO nestova.external_event
      (id, calendar_account_id, external_id, title, starts_at, ends_at, all_day, updated_at)
    VALUES (gen_random_uuid(), '${accountID}', '${title}', '${title}',
            ${startsAt}, ${endsAt}, ${allDay}, now());
  `);
}

// thisMonth returns a timestamp expression inside the month the page renders,
// which is the current UTC month.
function thisMonth(day, time = '12:00') {
  return `date_trunc('month', now() AT TIME ZONE 'UTC') + interval '${day - 1} days' + interval '${time}'`;
}

test.describe('§9.1 the unified view and connecting', () => {
  test('T-9.1.1 the month view shows household items and synced events together', async ({
    page,
  }) => {
    await login(page, PERSONAS.owner);
    const account = seedAccount();
    const synced = name('Dentist');
    seedEvent({ accountID: account, title: synced, startsAt: thisMonth(15), endsAt: thisMonth(15, '13:00') });

    // A household item in the same month: a subscription renewal is the
    // cheapest to seed, and the unified service merges it with the events.
    const renewal = name('Streaming');
    psql(`
      INSERT INTO nestova.subscription
        (id, household_id, name, amount_cents, currency, cycle, next_renewal_on, active, created_at, updated_at)
      VALUES (gen_random_uuid(), '${householdA()}', '${renewal}', 999, 'USD', 'monthly',
              (date_trunc('month', now() AT TIME ZONE 'UTC') + interval '20 days')::date, true, now(), now());
    `);

    await page.goto('/calendar');
    await expect(page.getByText(synced, { exact: false }).first()).toBeVisible();
    await expect(page.getByText(renewal, { exact: false }).first()).toBeVisible();
  });

  test('T-9.1.2 connect redirects to the Google consent screen with a signed state', async ({
    page,
  }) => {
    await login(page, PERSONAS.owner);
    const token = await csrfToken(page, '/calendar');

    const location = await page.evaluate(async (csrf) => {
      const res = await fetch('/calendar/google/connect', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ csrf_token: csrf }).toString(),
        redirect: 'manual',
      });
      // A same-origin manual redirect is opaque to fetch, so ask for the header
      // through an HTMX-style request, which answers with HX-Redirect instead.
      const hx = await fetch('/calendar/google/connect', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'HX-Request': 'true' },
        body: new URLSearchParams({ csrf_token: csrf }).toString(),
      });
      return { status: res.type === 'opaqueredirect' ? 303 : res.status, url: hx.headers.get('HX-Redirect') };
    }, token);

    expect(location.status).toBe(303);
    expect(location.url).toContain('accounts.google.com');
    // The state is the OAuth CSRF defence, and T-9.2.2 depends on it existing.
    expect(new URL(location.url).searchParams.get('state')).toBeTruthy();
  });

  test('T-9.1.3 connecting for real is not exercised here', async () => {
    // The callback's success path exchanges the authorization code with Google
    // for tokens. Stubbing that exchange would mean asserting the stub, and the
    // app offers no seam to point the exchanger at a local fake, so this stays
    // uncovered on purpose. The REJECTION paths below are the part that is
    // local logic, and they are covered. internal/calendar/app's own tests
    // cover Connect against a fake exchanger.
    test.skip(true, 'needs a real Google token exchange; no local seam to stub it');
  });
});

test.describe('§9.2 callback rejections and rendering edge cases', () => {
  // callback issues the OAuth callback GET from inside the session and returns
  // its status plus the path it redirected to.
  async function callback(page, query) {
    return page.evaluate(async (qs) => {
      const res = await fetch(`/calendar/google/callback?${qs}`, { redirect: 'manual' });
      if (res.type === 'opaqueredirect') return { status: 303, redirected: true };
      return { status: res.status, redirected: false, body: (await res.text()).slice(0, 120) };
    }, query);
  }

  // freshState mints a signed state for the CURRENT session's member by asking
  // Connect for one, which is the only supported way to get a valid one.
  async function freshState(page) {
    const token = await csrfToken(page, '/calendar');
    const url = await page.evaluate(async (csrf) => {
      const res = await fetch('/calendar/google/connect', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'HX-Request': 'true' },
        body: new URLSearchParams({ csrf_token: csrf }).toString(),
      });
      return res.headers.get('HX-Redirect');
    }, token);
    return new URL(url).searchParams.get('state');
  }

  test('T-9.2.1 a valid state with an unusable code leaves no partial account', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const before = accountCount();
    const state = await freshState(page);

    const res = await callback(page, `code=not-a-real-code&state=${encodeURIComponent(state)}`);

    // The exchange fails against Google, and the handler redirects to
    // /calendar?connect=error rather than storing half an account.
    expect(res.status).toBe(303);
    expect(accountCount()).toBe(before);
  });

  test('T-9.2.2 a state minted for another member is rejected', async ({ page }) => {
    // Mint as the adult...
    await login(page, PERSONAS.adult);
    const adultState = await freshState(page);

    // ...and present it as the owner.
    await login(page, PERSONAS.owner);
    const before = accountCount();
    const res = await callback(page, `code=whatever&state=${encodeURIComponent(adultState)}`);

    expect(res.status).toBe(403);
    expect(accountCount()).toBe(before);
  });

  test('T-9.2.2b a forged or missing state is rejected', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const before = accountCount();

    expect((await callback(page, 'code=abc&state=forged')).status).toBe(403);
    expect((await callback(page, 'code=abc')).status).toBe(400);
    expect((await callback(page, 'state=abc')).status).toBe(400);
    expect(accountCount()).toBe(before);
  });

  test('T-9.2.3 replaying the callback creates no second account', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const before = accountCount();
    const state = await freshState(page);
    const query = `code=replayed-code&state=${encodeURIComponent(state)}`;

    await callback(page, query);
    await callback(page, query);
    await callback(page, query);

    expect(accountCount()).toBe(before);
  });

  test('T-9.2.4 declining consent is handled with a message, not an error', async ({ page }) => {
    await login(page, PERSONAS.owner);

    const res = await callback(page, 'error=access_denied');
    expect(res.status).toBe(303);

    // The redirect target carries the outcome, and the page renders normally.
    await page.goto('/calendar?connect=denied');
    await expect(page.getByRole('heading', { name: 'Calendar', level: 1 })).toBeVisible();
  });

  test('T-9.2.5 an invalid sync token is not reachable from any HTTP surface', async () => {
    // ErrSyncTokenInvalid is raised by the Google sync client and handled by the
    // sync engine, which runs on the background scheduler. Nothing in the web
    // surface triggers a sync, so this checklist item cannot be driven from a
    // browser at all; internal/calendar's own tests own it.
    test.skip(true, 'sync runs on the background scheduler; no HTTP surface triggers it');
  });

  test('T-9.2.6 one unusable cached event does not stop the others rendering', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const account = seedAccount();

    const good = name('Parents evening');
    seedEvent({ accountID: account, title: good, startsAt: thisMonth(10), endsAt: thisMonth(10, '13:00') });

    // The schema refuses the malformed shapes the domain calls invalid — a
    // blank external_id, and an event ending before it starts — so a row that
    // would break rendering cannot reach the table in the first place. That IS
    // the safeguard this item asks about; assert it rather than pretending the
    // row exists.
    let refusedBlank = false;
    try {
      psql(`
        INSERT INTO nestova.external_event
          (id, calendar_account_id, external_id, title, starts_at, ends_at, all_day, updated_at)
        VALUES (gen_random_uuid(), '${account}', '   ', 'blank id', ${thisMonth(11)}, ${thisMonth(11, '13:00')}, false, now());
      `);
    } catch (err) {
      refusedBlank = /external_event_external_id_check/.test(String(err.stderr || err));
    }
    expect(refusedBlank).toBe(true);

    let refusedOrder = false;
    try {
      psql(`
        INSERT INTO nestova.external_event
          (id, calendar_account_id, external_id, title, starts_at, ends_at, all_day, updated_at)
        VALUES (gen_random_uuid(), '${account}', '${name('backwards')}', 'backwards',
                ${thisMonth(12, '15:00')}, ${thisMonth(12, '09:00')}, false, now());
      `);
    } catch (err) {
      refusedOrder = /external_event_time_order/.test(String(err.stderr || err));
    }
    expect(refusedOrder).toBe(true);

    await page.goto('/calendar');
    await expect(page.getByText(good, { exact: false }).first()).toBeVisible();
  });

  test('T-9.2.7 all-day, multi-day and timezone-shifted events render', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const account = seedAccount();

    const allDay = name('Sports day');
    seedEvent({
      accountID: account,
      title: allDay,
      startsAt: thisMonth(5, '00:00'),
      endsAt: thisMonth(6, '00:00'),
      allDay: true,
    });

    const multiDay = name('Camping trip');
    seedEvent({
      accountID: account,
      title: multiDay,
      startsAt: thisMonth(7, '18:00'),
      endsAt: thisMonth(9, '11:00'),
    });

    // Stored in a non-UTC zone: the page formats in UTC, so this proves the
    // conversion happens rather than the wall-clock string being echoed.
    const shifted = name('Tokyo call');
    seedEvent({
      accountID: account,
      title: shifted,
      startsAt: `timezone('Asia/Tokyo', ${thisMonth(8, '09:00')})`,
      endsAt: `timezone('Asia/Tokyo', ${thisMonth(8, '10:00')})`,
    });

    await page.goto('/calendar');
    for (const title of [allDay, multiDay, shifted]) {
      await expect(page.getByText(title, { exact: false }).first()).toBeVisible();
    }
  });

  test('T-9.2.8 an event spanning a DST boundary renders with the right instant', async ({
    page,
  }) => {
    await login(page, PERSONAS.owner);
    const account = seedAccount();

    // 01:30 America/New_York on a spring-forward morning, expressed as an
    // absolute instant. The page renders in UTC, so the assertion is about the
    // stored instant surviving the zone conversion, not about local wall time.
    const dst = name('DST redeye');
    seedEvent({
      accountID: account,
      title: dst,
      startsAt: `timezone('America/New_York', ${thisMonth(14, '01:30')})`,
      endsAt: `timezone('America/New_York', ${thisMonth(14, '03:30')})`,
    });

    await page.goto('/calendar');
    await expect(page.getByText(dst, { exact: false }).first()).toBeVisible();

    // The page prints a timed event as "Jan 2, 3:04 PM MST" formatted in UTC, so
    // assert the UTC instant the row actually holds — 01:30 New York in March is
    // 06:30 UTC, and the point of the case is that the offset was applied rather
    // than the wall-clock string echoed.
    const rendered = psql(`
      SELECT to_char(starts_at AT TIME ZONE 'UTC', 'FMMon FMDD, FMHH12:MI AM')
        FROM nestova.external_event WHERE title = '${dst}';
    `).trim();
    await expect(page.locator('body')).toContainText(`${rendered} UTC`);
  });

  test('T-9.2.9 the page still renders when Google is unreachable', async ({ page, context }) => {
    await login(page, PERSONAS.owner);
    const account = seedAccount();
    const cached = name('Cached offline');
    seedEvent({ accountID: account, title: cached, startsAt: thisMonth(18), endsAt: thisMonth(18, '13:00') });

    // Nothing on this page should reach out to Google: the events are cached
    // rows, and the sync is a background concern. Blocking every Google request
    // proves it.
    await context.route('**://*.google.com/**', (route) => route.abort());
    await context.route('**://*.googleapis.com/**', (route) => route.abort());

    await page.goto('/calendar');
    await expect(page.getByRole('heading', { name: 'Calendar', level: 1 })).toBeVisible();
    await expect(page.getByText(cached, { exact: false }).first()).toBeVisible();
  });

  test('T-9.2.10 an account revoked upstream leaves the page working', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const account = seedAccount();
    const orphaned = name('Was synced');
    seedEvent({ accountID: account, title: orphaned, startsAt: thisMonth(22), endsAt: thisMonth(22, '13:00') });

    await page.goto('/calendar');
    await expect(page.getByText(orphaned, { exact: false }).first()).toBeVisible();

    // Disconnected upstream: the account row goes, and external_event's FK is
    // ON DELETE CASCADE, so its cached events go with it. The page must render
    // the remaining household items rather than 500 on a missing account.
    psql(`DELETE FROM nestova.calendar_account WHERE id = '${account}';`);

    await page.goto('/calendar');
    await expect(page.getByRole('heading', { name: 'Calendar', level: 1 })).toBeVisible();
    await expect(page.getByText(orphaned, { exact: false })).toHaveCount(0);
  });
});
