// §5 chores: terminal-state guards, ownership, and the §0.7 races that live
// here. Instances are seeded directly because they are materialised by a
// background scheduler, so a chore created through the form has no actionable
// row for minutes — the assertions still run entirely against the HTTP surface.
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, postForm } = require('./helpers');
const { psql } = require('../tests/db');
const {
  newMember, signIn, seedInstance: seedChoreInstance, instanceRow, ledgerFor, balanceOf, taskRow,
} = require('./helpers-chores');

function householdA() {
  return psql("SELECT id FROM identity.household WHERE name = 'Household A' LIMIT 1;").trim();
}
function memberId(displayName) {
  return psql(`SELECT id FROM identity.member WHERE display_name = '${displayName}' LIMIT 1;`).trim();
}

function seedTask({ policy = 'claimable', points = 5 } = {}) {
  return psql(`
    INSERT INTO nestova.recurring_task
      (id, household_id, title, category, cadence, rotation_policy, points, lead_time_days, active, created_at, updated_at)
    VALUES (gen_random_uuid(), '${householdA()}', 'Chore probe', 'chore',
            '{"Freq":"daily","Interval":1,"Anchor":"2026-08-25T00:00:00Z","ByWeekday":null}'::jsonb,
            '${policy}', ${points}, 0, true, now(), now())
    RETURNING id;`).trim();
}

function seedInstance({ assignee, status = 'pending' } = {}) {
  const taskId = seedTask();
  const who = assignee ? `'${assignee}'` : 'NULL';
  return psql(`
    INSERT INTO nestova.task_instance
      (id, recurring_task_id, household_id, assignee_id, due_on, status, kind, created_at, updated_at)
    VALUES (gen_random_uuid(), '${taskId}', '${householdA()}', ${who}, current_date, '${status}', 'scheduled', now(), now())
    RETURNING id;`).trim();
}

function instanceStatus(id) {
  return psql(`SELECT status FROM nestova.task_instance WHERE id = '${id}';`).trim();
}

function ledgerCountFor(instanceId) {
  return Number(psql(
    `SELECT count(*) FROM nestova.point_ledger WHERE source_id = '${instanceId}';`,
  ).trim());
}

async function postConcurrently(page, path, fields, n) {
  return page.evaluate(async ({ path, fields, n }) => {
    const body = new URLSearchParams(fields).toString();
    const once = () => fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body, redirect: 'manual',
    }).then((r) => (r.type === 'opaqueredirect' ? 303 : r.status));
    return Promise.all(Array.from({ length: n }, once));
  }, { path, fields, n });
}

async function tasksToken(page) {
  await page.goto('/tasks');
  return page.locator('input[name="csrf_token"]').first().inputValue();
}

test.describe('§5 chores', () => {
  test('T-5.2.1 completing an assigned chore succeeds', async ({ page }) => {
    const owner = memberId(PERSONAS.owner.displayName);
    const id = seedInstance({ assignee: owner });
    await login(page, PERSONAS.owner);
    const csrf_token = await tasksToken(page);

    expect(await postForm(page, `/tasks/${id}/complete`, { csrf_token })).toBe(303);
    expect(instanceStatus(id), 'the instance must reach a completed state').not.toBe('pending');
  });

  test('T-5.2.4 completing an already-completed chore is refused', async ({ page }) => {
    const owner = memberId(PERSONAS.owner.displayName);
    const id = seedInstance({ assignee: owner });
    await login(page, PERSONAS.owner);
    const csrf_token = await tasksToken(page);

    expect(await postForm(page, `/tasks/${id}/complete`, { csrf_token })).toBe(303);
    const second = await postForm(page, `/tasks/${id}/complete`, { csrf_token });
    expect(second, 'a terminal instance must not accept a second completion').toBeGreaterThanOrEqual(400);
  });

  test('T-5.2.9 completing then skipping the same instance is refused', async ({ page }) => {
    const owner = memberId(PERSONAS.owner.displayName);
    const id = seedInstance({ assignee: owner });
    await login(page, PERSONAS.owner);
    const csrf_token = await tasksToken(page);

    expect(await postForm(page, `/tasks/${id}/complete`, { csrf_token })).toBe(303);
    expect(await postForm(page, `/tasks/${id}/skip`, { csrf_token })).toBeGreaterThanOrEqual(400);
  });

  test('T-0.7.2 [!] concurrent completions award points once', async ({ page }) => {
    const owner = memberId(PERSONAS.owner.displayName);
    const id = seedInstance({ assignee: owner });
    await login(page, PERSONAS.owner);
    const csrf_token = await tasksToken(page);

    const statuses = await postConcurrently(page, `/tasks/${id}/complete`, { csrf_token }, 5);
    const accepted = statuses.filter((s) => s === 303).length;

    expect(accepted, 'only one completion may be accepted').toBe(1);
    expect(ledgerCountFor(id), 'points must be awarded exactly once').toBe(1);
  });

  test('T-0.7.3 [!] two members claiming at once resolve to a single winner', async ({ browser }) => {
    const id = seedInstance({ assignee: null });

    // Two DIFFERENT members: one member re-claiming their own chore is
    // idempotent and proves nothing about the race.
    const ctxA = await browser.newContext();
    const ctxB = await browser.newContext();
    const pageA = await ctxA.newPage();
    const pageB = await ctxB.newPage();
    await login(pageA, PERSONAS.owner);
    await login(pageB, PERSONAS.adult);
    const tokenA = await tasksToken(pageA);
    const tokenB = await tasksToken(pageB);

    const [a, b] = await Promise.all([
      postForm(pageA, `/tasks/${id}/claim`, { csrf_token: tokenA }),
      postForm(pageB, `/tasks/${id}/claim`, { csrf_token: tokenB }),
    ]);

    const accepted = [a, b].filter((s) => s === 303).length;
    expect(accepted, 'exactly one of two racing members may win the claim').toBe(1);

    const claimants = psql(
      `SELECT count(DISTINCT claimed_by) FROM nestova.task_instance WHERE id = '${id}' AND claimed_by IS NOT NULL;`,
    ).trim();
    expect(claimants, 'exactly one claimant must be recorded').toBe('1');

    await ctxA.close();
    await ctxB.close();
  });

  test('T-5.2.5 completion is household-wide, and credits the member who did it', async ({ page }) => {
    // The checklist expected ErrNotYourChore here. The app has no such rule for
    // completion — CompleteInstance takes the actor as a parameter and records
    // it, so in a shared household anyone may complete anyone's chore and the
    // points follow the DOER, not the assignee. ErrNotYourChore guards trades
    // only. This test pins the behaviour that actually exists; see the run
    // notes in the checklist for the open design question.
    const adult = memberId(PERSONAS.adult.displayName);
    const child = memberId(PERSONAS.child.displayName);
    const id = seedInstance({ assignee: adult });

    await login(page, PERSONAS.child);
    const csrf_token = await tasksToken(page);
    expect(await postForm(page, `/tasks/${id}/complete`, { csrf_token })).toBe(303);

    const completedBy = psql(
      `SELECT completed_by FROM nestova.task_instance WHERE id = '${id}';`,
    ).trim();
    expect(completedBy, 'the completer, not the assignee, must be recorded').toBe(child);
  });

  test('T-5.2.7 an unknown instance id is refused without a 5xx', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const csrf_token = await tasksToken(page);
    const status = await postForm(page, '/tasks/00000000-0000-0000-0000-000000000000/complete', { csrf_token });
    expect(status).toBeGreaterThanOrEqual(400);
    expect(status, 'an unknown id must not produce a server error').toBeLessThan(500);
  });

  test('T-5.5.2 proposing a trade with yourself is refused', async ({ page }) => {
    const owner = memberId(PERSONAS.owner.displayName);
    const mine = seedInstance({ assignee: owner });
    const alsoMine = seedInstance({ assignee: owner });

    await login(page, PERSONAS.owner);
    const csrf_token = await tasksToken(page);
    const status = await postForm(page, '/trades', {
      csrf_token, offered_instance_id: mine, requested_instance_id: alsoMine,
    });
    expect(status, 'a self-trade must be refused').toBeGreaterThanOrEqual(400);
  });

  test('T-5.5.1 proposing a trade on a chore that is not yours is refused', async ({ page }) => {
    const adult = memberId(PERSONAS.adult.displayName);
    const child = memberId(PERSONAS.child.displayName);
    const notMine = seedInstance({ assignee: adult });
    const theirs = seedInstance({ assignee: child });

    // Owner offers a chore assigned to the adult — not theirs to offer.
    await login(page, PERSONAS.owner);
    const csrf_token = await tasksToken(page);
    const status = await postForm(page, '/trades', {
      csrf_token, offered_instance_id: notMine, requested_instance_id: theirs,
    });
    expect(status, 'offering a chore you are not assigned must be refused').toBeGreaterThanOrEqual(400);
  });

  test('T-5.5.4/T-5.5.5 a trade can be proposed and cancelled, and cancelling twice is refused', async ({ page }) => {
    const owner = memberId(PERSONAS.owner.displayName);
    const child = memberId(PERSONAS.child.displayName);
    const mine = seedInstance({ assignee: owner });
    const theirs = seedInstance({ assignee: child });

    await login(page, PERSONAS.owner);
    const csrf_token = await tasksToken(page);
    const proposed = await postForm(page, '/trades', {
      csrf_token, offered_instance_id: mine, requested_instance_id: theirs,
    });
    expect(proposed, 'a valid trade proposal must be accepted').toBe(303);

    const tradeId = psql(
      `SELECT id FROM nestova.chore_trade WHERE offered_instance_id = '${mine}' LIMIT 1;`,
    ).trim();
    expect(tradeId, 'the trade should exist').toBeTruthy();

    expect(await postForm(page, `/trades/${tradeId}/cancel`, { csrf_token })).toBe(303);
    const again = await postForm(page, `/trades/${tradeId}/cancel`, { csrf_token });
    expect(again, 'a resolved trade must not be cancelled again').toBeGreaterThanOrEqual(400);
  });
});

// §5.2 actions through the real row controls. Each acts as a fresh member so a
// PIN another spec enrolled on a shared persona cannot change the outcome.
test.describe('§5.2 complete / skip / claim', () => {
  // markDocument tags the live document so a test can prove an action swapped
  // the row without a navigation: a full reload would drop the tag.
  async function markDocument(page) {
    await page.evaluate(() => { window.__noReload = true; });
  }
  async function expectNoReload(page) {
    expect(await page.evaluate(() => window.__noReload === true), 'the page must not reload').toBe(true);
    expect(new URL(page.url()).pathname).toBe('/tasks');
  }

  test('T-5.2.2 skip swaps the row in place, through the NES-166 PIN gate', async ({ page }) => {
    const member = newMember('Skipper');
    const inst = seedChoreInstance({ assignee: member.id });
    await signIn(page, member);
    const csrf_token = await tasksToken(page);
    expect(await postForm(page, '/settings/pin', { csrf_token, pin: '2468' })).toBe(200);

    await page.goto('/tasks');
    await markDocument(page);
    const row = taskRow(page, inst.id);
    await row.locator('[data-testid="task-pin-input"]').fill('2468');
    const skipped = page.waitForResponse((r) => r.url().endsWith(`/tasks/${inst.id}/skip`) && r.request().method() === 'POST');
    await row.getByRole('button', { name: 'Skip' }).click();
    expect((await skipped).status()).toBe(200);

    await expect(taskRow(page, inst.id)).toContainText('Skipped');
    await expect(taskRow(page, inst.id).getByRole('button', { name: 'Done' })).toHaveCount(0);
    await expectNoReload(page);
    expect(instanceRow(inst.id).status).toBe('skipped');
  });

  test('T-5.2.3 claim swaps the row in place and shows the countdown', async ({ page }) => {
    const member = newMember('Grabber');
    const inst = seedChoreInstance();
    await signIn(page, member);
    await page.goto('/tasks');
    await markDocument(page);

    const claimed = page.waitForResponse((r) => r.url().endsWith(`/tasks/${inst.id}/claim`) && r.request().method() === 'POST');
    await taskRow(page, inst.id).getByRole('button', { name: 'Claim' }).click();
    expect((await claimed).status()).toBe(200);

    const row = taskRow(page, inst.id);
    await expect(row).toContainText(/expires in (12h 0m|11h 59m)/);
    await expect(row.getByRole('button', { name: 'Done' })).toBeVisible();
    await expect(row.getByRole('button', { name: 'Claim' })).toHaveCount(0);
    await expectNoReload(page);
    expect(instanceRow(inst.id).claimedBy).toBe(member.id);
  });

  test('T-5.2.6 claiming a chore another member already claimed is refused with 409', async ({ browser }) => {
    const first = newMember('FirstClaim');
    const second = newMember('SecondClaim');
    const inst = seedChoreInstance();

    const ctxA = await browser.newContext();
    const ctxB = await browser.newContext();
    const pageA = await ctxA.newPage();
    const pageB = await ctxB.newPage();
    await signIn(pageA, first);
    await signIn(pageB, second);
    const tokenA = await tasksToken(pageA);
    const tokenB = await tasksToken(pageB);

    expect(await postForm(pageA, `/tasks/${inst.id}/claim`, { csrf_token: tokenA }), 'sanity: the first claim wins').toBe(303);
    const refused = await pageB.evaluate(async ({ path, token }) => {
      const res = await fetch(path, {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ csrf_token: token }).toString(),
        redirect: 'manual',
      });
      return { status: res.status, body: await res.text() };
    }, { path: `/tasks/${inst.id}/claim`, token: tokenB });

    expect(refused.status, 'ErrInstanceAlreadyClaimed maps to 409').toBe(409);
    expect(refused.body).toContain('task already acted on');
    expect(instanceRow(inst.id).claimedBy, 'the first claimant keeps the chore').toBe(first.id);

    await ctxA.close();
    await ctxB.close();
  });

  test('T-5.2.8 a completion awards its points exactly once', async ({ page }) => {
    const member = newMember('Earner');
    const inst = seedChoreInstance({ points: 7 });
    await signIn(page, member);
    const csrf_token = await tasksToken(page);

    expect(await postForm(page, `/tasks/${inst.id}/complete`, { csrf_token })).toBe(303);
    expect(ledgerFor(inst.id)).toEqual([{ memberId: member.id, sourceType: 'task_instance', points: 7 }]);

    expect(await postForm(page, `/tasks/${inst.id}/complete`, { csrf_token }), 'a second completion is refused').toBe(409);
    expect(ledgerFor(inst.id), 'the refused completion must not add a ledger row').toHaveLength(1);
    expect(balanceOf(member.id)).toBe(7);

    await page.goto('/rewards');
    const balanceCard = page.locator('div').filter({ has: page.getByRole('heading', { name: 'Your Balance' }) }).last();
    await expect(balanceCard).toContainText('7 pts');
  });
});
