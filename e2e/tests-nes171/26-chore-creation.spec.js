// §5.1 chore creation: POST /tasks validation, each cadence, and the calendar
// edge cases.
//
// Negative cases assert the exact validation message as well as the 422, and
// every describe proves a fully valid payload is accepted by the same route, so
// a refusal for the wrong reason (a missing field) cannot pass as the refusal
// under test.
//
// Occurrence dates (the 31st in February, Feb 29 in a non-leap year) only exist
// once the background generator has materialised instances, which happens on
// the 5-minute task scheduler tick. Those items live in 27-claims.spec.js's
// scheduler describe, which is opt-in (NES171_SCHEDULER_WAIT=1).
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login } = require('./helpers');
const { psql } = require('../tests/db');
const {
  newMember, todayISO, daysFromTodayISO, tasksToken, postPairs, choreForm, titleOf, storedTask,
} = require('./helpers-chores');

const INTERVAL_MESSAGE = 'Interval must be a whole number between 1 and 1000.';

// create posts a chore form as the signed-in member and returns the response
// plus the task it persisted (null when nothing was stored).
async function create(page, csrf, overrides) {
  const pairs = choreForm(csrf, overrides);
  const res = await postPairs(page, '/tasks', pairs);
  const title = titleOf(pairs);
  return { ...res, title, task: storedTask(title) };
}

async function expectRefused(result, message) {
  expect(result.status, `expected 422 "${message}"`).toBe(422);
  expect(result.body).toContain(message);
  expect(result.task, 'a refused chore must not be persisted').toBeNull();
}

test.describe('§5.1 chore creation', () => {
  let csrf;

  test.beforeEach(async ({ page }) => {
    await login(page, PERSONAS.owner);
    csrf = await tasksToken(page);
  });

  test('T-5.1.1 a daily chore created through the form appears on /tasks', async ({ page }) => {
    const member = newMember('Creator');
    const title = `Daily dishes ${Date.now()}`;

    await page.goto('/tasks/new');
    await page.locator('input[name="title"]').fill(title);
    await page.locator('input[name="category"][value="chore"]').check();
    await page.locator('select[name="freq"]').selectOption('daily');
    await page.locator('input[name="interval"]').fill('1');
    await page.locator('input[name="anchor"]').fill(todayISO());
    await page.locator('select[name="rotation_policy"]').selectOption('fixed');
    await page.locator('label', { hasText: member.displayName }).locator('input[name="pool"]').check();
    await page.locator('input[name="points"]').fill('3');
    await page.getByRole('button', { name: 'Save chore' }).click();
    await page.waitForURL((u) => new URL(u).pathname === '/tasks');
    await expect(page.getByRole('alert')).toHaveCount(0);

    const task = storedTask(title);
    expect(task, 'the chore must be persisted').not.toBeNull();
    expect(task.cadence.Freq).toBe('daily');

    // Materialise today's instance the way the 5-minute generator would —
    // assigned to the chore's single pool member — then assert it renders.
    psql(`
      INSERT INTO nestova.task_instance (id, household_id, recurring_task_id, assignee_id, due_on, status, kind)
      SELECT gen_random_uuid(), t.household_id, t.id, rm.member_id, current_date, 'pending', 'scheduled'
        FROM nestova.recurring_task t JOIN nestova.rotation_member rm ON rm.recurring_task_id = t.id
       WHERE t.id = '${task.id}';`);
    await page.goto('/tasks');
    const group = page.locator('section').filter({ has: page.getByRole('heading', { name: member.displayName }) });
    await expect(group.getByText(title)).toBeVisible();
  });

  test('T-5.1.2 every supported cadence is accepted, and "yearly" is not a cadence', async ({ page }) => {
    const accepted = {};
    for (const [freq, extra] of [
      ['daily', {}],
      ['weekly', { byweekday: ['1'] }],
      ['monthly', {}],
      ['as_needed', { interval: null, anchor: null }],
    ]) {
      const r = await create(page, csrf, { freq, ...extra });
      accepted[freq] = r.status === 303 && r.task && r.task.cadence.Freq === freq;
    }
    expect(accepted).toEqual({ daily: true, weekly: true, monthly: true, as_needed: true });

    // CHECKLIST WRONG: there is no yearly frequency. household.Freq defines
    // daily/weekly/monthly/as_needed only (internal/household/domain/cadence.go),
    // the form offers no Yearly option, and a yearly chore is expressed as
    // monthly with interval 12.
    await expectRefused(await create(page, csrf, { freq: 'yearly' }), 'Please select a valid frequency.');
    const everyTwelveMonths = await create(page, csrf, { freq: 'monthly', interval: '12' });
    expect(everyTwelveMonths.status).toBe(303);
    expect(everyTwelveMonths.task.cadence).toMatchObject({ Freq: 'monthly', Interval: 12 });
  });

  test('T-5.1.3 an empty or whitespace-only title is refused with 422', async ({ page }) => {
    await expectRefused(await create(page, csrf, { title: '' }), 'Title is required.');
    const blank = await postPairs(page, '/tasks', choreForm(csrf, { title: '   \t ' }));
    expect(blank.status).toBe(422);
    expect(blank.body).toContain('Title is required.');

    expect((await create(page, csrf, {})).status, 'sanity: a titled chore is accepted').toBe(303);
  });

  test('T-5.1.4 a zero or negative interval is refused with 422', async ({ page }) => {
    await expectRefused(await create(page, csrf, { interval: '0' }), INTERVAL_MESSAGE);
    await expectRefused(await create(page, csrf, { interval: '-1' }), INTERVAL_MESSAGE);
    await expectRefused(await create(page, csrf, { interval: '-9999' }), INTERVAL_MESSAGE);

    expect((await create(page, csrf, { interval: '1' })).status, 'sanity: interval 1 is accepted').toBe(303);
  });

  test('T-5.1.5 interval 9999 is refused; the upper bound is 1000 inclusive', async ({ page }) => {
    await expectRefused(await create(page, csrf, { interval: '9999' }), INTERVAL_MESSAGE);
    await expectRefused(await create(page, csrf, { interval: '1001' }), INTERVAL_MESSAGE);

    const max = await create(page, csrf, { interval: '1000' });
    expect(max.status, 'sanity: the maximum interval is accepted').toBe(303);
    expect(max.task.cadence.Interval).toBe(1000);
  });

  test('T-5.1.6 weekly with no weekday is accepted and recurs on the start date\'s weekday', async ({ page }) => {
    // CHECKLIST WRONG: an empty weekday set is a documented cadence, not an
    // error. household.Cadence: "with an empty ByWeekday they fall every
    // Interval weeks on the Anchor's own weekday" (cadence.go:65-67), so the
    // chore recurs weekly on the weekday of its start date.
    const noDays = await create(page, csrf, { freq: 'weekly', byweekday: null });
    expect(noDays.status).toBe(303);
    expect(noDays.task.cadence.ByWeekday ?? []).toEqual([]);
    expect(noDays.task.cadence.Anchor.slice(0, 10)).toBe(todayISO());

    // An out-of-range weekday, by contrast, is refused.
    const bad = await postPairs(page, '/tasks', choreForm(csrf, { freq: 'weekly', byweekday: ['7'] }));
    expect(bad.status).toBe(422);
    expect(bad.body).toContain('One or more selected weekday values are invalid.');
  });

  test('T-5.1.7 weekly on all seven weekdays is accepted', async ({ page }) => {
    const all = await create(page, csrf, { freq: 'weekly', byweekday: ['0', '1', '2', '3', '4', '5', '6'] });
    expect(all.status).toBe(303);
    expect([...all.task.cadence.ByWeekday].sort()).toEqual([0, 1, 2, 3, 4, 5, 6]);
  });

  test('T-5.1.10 an as-needed chore that is not claimable is refused', async ({ page }) => {
    const member = newMember('AsNeeded');
    for (const policy of ['fixed', 'round_robin']) {
      await expectRefused(
        await create(page, csrf, { freq: 'as_needed', interval: null, anchor: null, rotation_policy: policy, pool: [member.id] }),
        'As-needed chores must use the claimable assignment policy.',
      );
    }

    const claimable = await create(page, csrf, { freq: 'as_needed', interval: null, anchor: null, rotation_policy: 'claimable' });
    expect(claimable.status, 'sanity: a claimable as-needed chore is accepted').toBe(303);
    // Its single standing instance exists at once, unlike a scheduled chore's.
    await page.goto('/tasks');
    const anytime = page.locator('section').filter({ has: page.getByRole('heading', { name: 'Anytime' }) });
    await expect(anytime.getByText(claimable.title)).toBeVisible();
  });

  test('T-5.1.11 a rotation with an empty pool is refused', async ({ page }) => {
    const message = 'At least one rotation pool member is required for fixed or round-robin tasks.';
    await expectRefused(await create(page, csrf, { rotation_policy: 'fixed', pool: null }), message);
    await expectRefused(await create(page, csrf, { rotation_policy: 'round_robin', pool: null }), message);

    const member = newMember('Pool');
    const withPool = await create(page, csrf, { rotation_policy: 'round_robin', pool: [member.id] });
    expect(withPool.status, 'sanity: a rotation with a pool member is accepted').toBe(303);
    expect(psql(`SELECT count(*) FROM nestova.rotation_member WHERE recurring_task_id = '${withPool.task.id}';`).trim()).toBe('1');
  });

  test('T-5.1.12 a rotation member removed from the household afterwards', async () => {
    test.skip(true, 'Not built: Nestova has no member-removal route (only GET /members/new and POST /members '
      + 'in cmd/server/home.go), so a member cannot leave the household; and generation only runs on the '
      + '5-minute task scheduler tick.');
  });

  test('T-5.1.13 [!] a 10,000-character title is refused; 200 characters is the bound', async ({ page }) => {
    const tooLong = 'Title must be 200 characters or fewer.';
    await expectRefused(await create(page, csrf, { title: 'x'.repeat(10_000) }), tooLong);
    await expectRefused(await create(page, csrf, { title: 'y'.repeat(201) }), tooLong);

    // The bound counts characters (runes), not bytes: 200 four-byte emoji fit.
    const suffix = String(Date.now());
    const atLimit = await create(page, csrf, { title: `${suffix}${'z'.repeat(200 - suffix.length)}` });
    expect(atLimit.status, 'sanity: a 200-character title is accepted').toBe(303);
    const emoji = await create(page, csrf, { title: `${suffix}${'🧹'.repeat(200 - suffix.length)}` });
    expect(emoji.status, '200 multi-byte characters are within the bound').toBe(303);
  });

  test('T-5.1.14 a start date in the past or in year 3000 is accepted and stored as given', async ({ page }) => {
    // Defined behaviour: the anchor has no range check beyond a parseable
    // YYYY-MM-DD. A past anchor makes the generator backfill every occurrence
    // since then as overdue (asserted in 27-claims.spec.js's scheduler
    // describe); a far-future anchor materialises nothing until its date.
    const past = daysFromTodayISO(-2);
    const pastChore = await create(page, csrf, { anchor: past });
    expect(pastChore.status).toBe(303);
    expect(pastChore.task.cadence.Anchor.slice(0, 10)).toBe(past);

    const future = await create(page, csrf, { anchor: '3000-01-01' });
    expect(future.status).toBe(303);
    expect(future.task.cadence.Anchor.slice(0, 10)).toBe('3000-01-01');

    // A year that does not fit the date format is refused, not a 500.
    const unparseable = await postPairs(page, '/tasks', choreForm(csrf, { anchor: '10000-01-01' }));
    expect(unparseable.status).toBe(422);
    expect(unparseable.body).toContain('Starting date must be in YYYY-MM-DD format.');
  });
});
