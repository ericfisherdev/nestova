// §5.3 claims and expiry, plus the §5.1 occurrence-date items that only the
// generator can show.
//
// A claim made off "Up for grabs" carries a 12-hour window
// (tasks/domain.ClaimWindow). The countdown badge is client-side
// (web/static/js/claim-countdown.js); the warning, the revert and the penalty
// all run on the background task scheduler, which ticks every 5 minutes
// (cmd/server/main.go taskSchedulerPollInterval) with no HTTP trigger.
//
// Claims are seeded with back-dated claimed_at/claim_expires_at so a test can
// stand at any point of the window. The scheduler-dependent items are in the
// last describe, which waits for one real tick and is therefore opt-in:
//
//   NES171_SCHEDULER_WAIT=1 npx playwright test -c nes171.config.js \
//     --project=checklist tests-nes171/27-claims.spec.js
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, postForm } = require('./helpers');
const { psql } = require('../tests/db');
const {
  newMember, signIn, todayISO, daysFromTodayISO, seedInstance, seedClaim, instanceRow, ledgerFor,
  balanceOf, tasksToken, taskRow, postPairs, choreForm, titleOf, storedTask,
} = require('./helpers-chores');

const SCHEDULER_WAIT = process.env.NES171_SCHEDULER_WAIT === '1';
const SCHEDULER_SKIP_REASON = 'Needs one real tick of the background task scheduler, which polls every 5 minutes '
  + '(cmd/server/main.go taskSchedulerPollInterval) and has no HTTP trigger. Run with NES171_SCHEDULER_WAIT=1.';

function claimWindowOf(id) {
  return psql(`SELECT claim_expires_at - claimed_at FROM nestova.task_instance WHERE id = '${id}';`).trim();
}

test.describe('§5.3 claims', () => {
  test('T-5.3.1 a claim holds for exactly 12 hours', async ({ page }) => {
    const member = newMember('Claimer');
    const inst = seedInstance();
    await signIn(page, member);
    const csrf_token = await tasksToken(page);

    expect(await postForm(page, `/tasks/${inst.id}/claim`, { csrf_token })).toBe(303);
    const row = instanceRow(inst.id);
    expect(row.claimedBy).toBe(member.id);
    expect(claimWindowOf(inst.id), 'claim_expires_at must be claimed_at + 12h').toBe('12:00:00');

    // Re-claiming your own claim must not restart the window.
    const before = instanceRow(inst.id).expires;
    expect(await postForm(page, `/tasks/${inst.id}/claim`, { csrf_token })).toBe(303);
    expect(instanceRow(inst.id).expires, 're-claiming must not extend the window').toBe(before);
  });

  test('T-5.3.2 the countdown renders on a claimed row', async ({ page }) => {
    const member = newMember('Countdown');
    const inst = seedClaim({ claimant: member.id, expiresIn: '3 hours 30 minutes' });
    await signIn(page, member);
    await page.goto('/tasks');

    await expect(taskRow(page, inst.id)).toContainText(/expires in 3h (30|29)m/);

    // A claim with no expiry risk (the chore was already yours) shows none.
    const own = seedInstance({ assignee: member.id });
    await page.goto('/tasks');
    await expect(taskRow(page, own.id)).toBeVisible();
    await expect(taskRow(page, own.id)).not.toContainText('expires in');
  });

  test('sanity: completing a claim at 11:59 of its window carries no penalty', async ({ page }) => {
    // The in-window half of T-5.3.5, kept separate so the expected-to-fail
    // T-5.3.5 test below cannot mask a regression here.
    const member = newMember('OnTime');
    const inst = seedClaim({ claimant: member.id, expiresIn: '1 minute', points: 10 });
    await signIn(page, member);
    const csrf_token = await tasksToken(page);

    expect(await postForm(page, `/tasks/${inst.id}/complete`, { csrf_token })).toBe(303);
    expect(ledgerFor(inst.id)).toEqual([{ memberId: member.id, sourceType: 'task_instance', points: 10 }]);
    expect(balanceOf(member.id)).toBe(10);
  });

  test('T-5.3.5 completing a claim at 12:01 treats it as expired', async ({ page }) => {
    test.fail(true, 'DEFECT: a lapsed claim completes penalty-free with full points until the 5-minute sweep reverts it');
    const member = newMember('Late');
    const inst = seedClaim({ claimant: member.id, expiresIn: '-1 minute', points: 10 });
    await signIn(page, member);
    const csrf_token = await tasksToken(page);

    const status = await postForm(page, `/tasks/${inst.id}/complete`, { csrf_token });
    const penalties = ledgerFor(inst.id).filter((e) => e.sourceType === 'claim_expiry');
    const sweptFirst = psql(`
      SELECT count(*) FROM nestova.point_ledger l JOIN nestova.task_instance t ON t.id = l.source_id
       WHERE l.source_id = '${inst.id}' AND l.source_type = 'claim_expiry' AND l.created_at < t.completed_at;`).trim();
    test.skip(sweptFirst !== '0', 'the background sweep reverted the claim before the completion landed');

    // Expired means one of two things, and the app does neither: the
    // completion is refused, or the claimant still incurs the lapse penalty.
    expect(status === 303 && penalties.length === 0,
      `completion returned ${status}, penalties recorded: ${penalties.length}`).toBe(false);
  });

  test.describe('T-5.3.8 countdown across a DST boundary', () => {
    test.use({ timezoneId: 'America/Chicago' });

    // Each case: the page's clock stands before the transition and the claim
    // expires after it, so wall-clock hours and real hours differ by one.
    for (const { name, now, expiresAt, label, offsetBefore, offsetAfter } of [
      // 01:00 CDT -> 07:00 CST: 6 wall-clock hours, 7 real hours.
      { name: 'fall back', now: '2026-11-01T06:00:00Z', expiresAt: '2026-11-01T13:00:00Z', label: '7h 0m', offsetBefore: 300, offsetAfter: 360 },
      // 01:00 CST -> 08:00 CDT: 7 wall-clock hours, 6 real hours.
      { name: 'spring forward', now: '2027-03-14T07:00:00Z', expiresAt: '2027-03-14T13:00:00Z', label: '6h 0m', offsetBefore: 360, offsetAfter: 300 },
    ]) {
      test(`T-5.3.8 the countdown counts real hours across a ${name}`, async ({ page }) => {
        const member = newMember('Dst');
        const inst = seedClaim({ claimant: member.id, expiresAt });
        await signIn(page, member);

        await page.clock.install({ time: new Date(now) });
        await page.goto('/tasks');
        const row = taskRow(page, inst.id);
        expect(await page.evaluate(() => new Date().getTimezoneOffset())).toBe(offsetBefore);
        await expect(row).toContainText(`expires in ${label}`);

        // One minute before the real expiry, on the far side of the change.
        const realMinutes = (Date.parse(expiresAt) - Date.parse(now)) / 60000;
        await page.clock.fastForward((realMinutes - 1) * 60000);
        expect(await page.evaluate(() => new Date().getTimezoneOffset()), 'the clock crossed the DST change').toBe(offsetAfter);
        await expect(row).toContainText('expires in 1m');

        await page.clock.fastForward(2 * 60000);
        await expect(taskRow(page, inst.id)).toContainText('Claim expiring…');
      });
    }
  });
});

// Everything below needs the scheduler to run once. beforeAll seeds every
// state, waits for the tick that sweeps the seeded expired claims (the
// generator, warning and expiry steps all run in that same RunOnce), and each
// test then asserts one item's outcome.
test.describe.serial('§5.3/§5.1 after a scheduler tick (opt-in)', () => {
  test.skip(!SCHEDULER_WAIT, SCHEDULER_SKIP_REASON);

  const s = {};

  test.beforeAll(async ({ browser }) => {
    test.setTimeout(8 * 60_000);
    const page = await browser.newPage();
    await login(page, PERSONAS.owner);
    const csrf = await tasksToken(page);
    const createChore = async (overrides) => {
      const pairs = choreForm(csrf, overrides);
      expect((await postPairs(page, '/tasks', pairs)).status).toBe(303);
      return storedTask(titleOf(pairs));
    };

    // T-5.3.3: one claim inside the 2-hour warning window, one outside it.
    s.warnMember = newMember('Warned');
    s.warnSoon = seedClaim({ claimant: s.warnMember.id, expiresIn: '90 minutes' });
    s.warnLater = seedClaim({ claimant: s.warnMember.id, expiresIn: '3 hours' });

    // T-5.3.4: a member who has earned 20 points, then lets a 10-point claim lapse.
    s.penalised = newMember('Lapsed');
    const earned = seedInstance({ points: 20 });
    const lapsedPage = await browser.newPage();
    await signIn(lapsedPage, s.penalised);
    const lapsedCsrf = await tasksToken(lapsedPage);
    expect(await postForm(lapsedPage, `/tasks/${earned.id}/complete`, { csrf_token: lapsedCsrf })).toBe(303);
    await lapsedPage.close();
    s.lapsed = seedClaim({ claimant: s.penalised.id, expiresIn: '-1 minute', points: 10 });

    // T-5.3.6: a member with a zero balance lets a 3-point claim lapse.
    s.broke = newMember('Broke');
    s.brokeClaim = seedClaim({ claimant: s.broke.id, expiresIn: '-1 minute', points: 3 });

    // T-5.3.7: a lapsed claim someone else will pick up after the revert.
    s.reclaimFrom = newMember('Dropper');
    s.reclaim = seedClaim({ claimant: s.reclaimFrom.id, expiresIn: '-1 minute', points: 4 });

    // T-5.1.8 / T-5.1.9 / T-5.1.14 and the plain daily chore: the generator
    // materialises every occurrence from the anchor up to today + 14 days.
    s.monthly31 = await createChore({ freq: 'monthly', interval: '1', anchor: '2026-01-31' });
    s.leapDay = await createChore({ freq: 'monthly', interval: '12', anchor: '2024-02-29' });
    s.pastDaily = await createChore({ anchor: daysFromTodayISO(-2) });
    s.year3000 = await createChore({ anchor: '3000-01-01' });
    await page.close();

    // Wait for the tick: it reverts all three seeded lapsed claims together.
    const lapsedIds = [s.lapsed.id, s.brokeClaim.id, s.reclaim.id].map((id) => `'${id}'`).join(',');
    await expect.poll(
      () => psql(`SELECT count(*) FROM nestova.task_instance WHERE id IN (${lapsedIds}) AND claim_expires_at IS NOT NULL;`).trim(),
      { timeout: 6 * 60_000, intervals: [5_000] },
    ).toBe('0');
  });

  test.afterAll(() => {
    // The backfilled chores would otherwise keep their overdue rows on /tasks.
    const ids = [s.monthly31, s.leapDay, s.pastDaily, s.year3000].filter(Boolean).map((t) => `'${t.id}'`);
    if (ids.length) psql(`DELETE FROM nestova.recurring_task WHERE id IN (${ids.join(',')});`);
  });

  function dueDates(taskId) {
    const out = psql(`SELECT due_on FROM nestova.task_instance WHERE recurring_task_id = '${taskId}' ORDER BY due_on;`).trim();
    return out ? out.split('\n') : [];
  }

  test('T-5.3.3 the warning fires once a claim enters its final 2 hours', async () => {
    expect(instanceRow(s.warnSoon.id).warned, 'a claim 90 minutes from expiry is warned').not.toBe('');
    expect(instanceRow(s.warnLater.id).warned, 'a claim 3 hours from expiry is not warned yet').toBe('');
    const notices = psql(`
      SELECT title FROM nestova.notification
       WHERE member_id = '${s.warnMember.id}' AND source_id IN ('${s.warnSoon.id}', '${s.warnLater.id}')
         AND title LIKE 'Claim expiring soon:%';`).trim();
    expect(notices, 'exactly one warning, for the claim inside the window').toBe(`Claim expiring soon: ${s.warnSoon.title}`);
  });

  test('T-5.3.4 a lapsed claim reverts and applies a point penalty', async ({ page }) => {
    const row = instanceRow(s.lapsed.id);
    expect(row.assignee, 'the chore returns to "Up for grabs"').toBe('');
    expect(row.status).toBe('pending');
    // Penalty = half the chore's points, floor 1 (tasks/domain.ClaimExpiryPenalty).
    expect(ledgerFor(s.lapsed.id)).toEqual([{ memberId: s.penalised.id, sourceType: 'claim_expiry', points: -5 }]);
    expect(balanceOf(s.penalised.id)).toBe(15);

    await signIn(page, s.penalised);
    await page.goto('/rewards');
    const balanceCard = page.locator('div').filter({ has: page.getByRole('heading', { name: 'Your Balance' }) }).last();
    await expect(balanceCard).toContainText('15 pts');
  });

  test('T-5.3.6 a lapse penalty never takes a balance below zero', async () => {
    // Product decision 2026-09-29: balances have a floor of zero. The penalty
    // is capped at the member's balance, so a member with nothing to lose gets
    // no penalty row at all.
    expect(ledgerFor(s.brokeClaim.id), 'no penalty row against a zero balance').toEqual([]);
    expect(balanceOf(s.broke.id)).toBe(0);
  });

  test('T-5.3.7 claim, expire, then re-claim works', async ({ page }) => {
    expect(instanceRow(s.reclaim.id).assignee).toBe('');
    const taker = newMember('Taker');
    await signIn(page, taker);
    const csrf_token = await tasksToken(page);
    await expect(taskRow(page, s.reclaim.id).getByRole('button', { name: 'Claim' })).toBeVisible();

    expect(await postForm(page, `/tasks/${s.reclaim.id}/claim`, { csrf_token })).toBe(303);
    expect(instanceRow(s.reclaim.id).claimedBy).toBe(taker.id);
    expect(claimWindowOf(s.reclaim.id), 'the new claim gets a fresh 12-hour window').toBe('12:00:00');
  });

  test('T-5.1.8 monthly on the 31st lands on the last day of short months, skipping none', async () => {
    const today = todayISO();
    const expected = [];
    for (let m = 0; ; m++) {
      const lastDay = new Date(Date.UTC(2026, m + 1, 0)).getUTCDate();
      const due = new Date(Date.UTC(2026, m, Math.min(31, lastDay))).toISOString().slice(0, 10);
      if (due > today) break;
      expected.push(due);
    }
    const actual = dueDates(s.monthly31.id).filter((d) => d <= today);
    expect(actual).toEqual(expected);
    expect(actual).toContain('2026-02-28');
  });

  test('T-5.1.9 a Feb 29 yearly chore falls on Feb 28 in non-leap years', async () => {
    // "Yearly" is monthly with interval 12 (see T-5.1.2); the clamp is applied
    // from the anchor each time, so a leap year returns to the 29th.
    expect(dueDates(s.leapDay.id)).toEqual(['2024-02-29', '2025-02-28', '2026-02-28']);
  });

  test('extra: a past start date backfills overdue rows; a year-3000 start materialises nothing', async () => {
    const dates = dueDates(s.pastDaily.id);
    expect(dates[0]).toBe(daysFromTodayISO(-2));
    expect(dates).toContain(todayISO());
    const overdue = psql(`
      SELECT count(*) FROM nestova.task_instance
       WHERE recurring_task_id = '${s.pastDaily.id}' AND due_on < '${todayISO()}' AND status = 'overdue';`).trim();
    expect(overdue).toBe('2');
    expect(dueDates(s.year3000.id)).toEqual([]);
  });
});
