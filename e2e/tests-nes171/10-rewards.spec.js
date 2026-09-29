// §6 rewards and points, and the §0.7 concurrency cases that live here.
//
// The concurrency tests are the point of this file. Redeeming spends a
// balance and decrements stock, so a race that is not serialised shows up as
// a negative balance or an oversold reward — the kind of defect that only
// appears under simultaneous requests, never in a sequential click-through.
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, postForm } = require('./helpers');
const { psql } = require('../tests/db');

function householdA() {
  return psql("SELECT id FROM identity.household WHERE name = 'Household A' LIMIT 1;").trim();
}

function memberId(displayName) {
  return psql(`SELECT id FROM identity.member WHERE display_name = '${displayName}' LIMIT 1;`).trim();
}

// grantPoints tops a member up by writing a ledger entry directly. Earning
// points through the UI needs a materialised task instance and a completion,
// which is a different test's subject.
function grantPoints(member, points) {
  psql(`
    INSERT INTO nestova.point_ledger (id, household_id, member_id, source_type, source_id, points, created_at)
    VALUES (gen_random_uuid(), '${householdA()}', '${member}', 'adjustment', gen_random_uuid(), ${points}, now());
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
    VALUES (gen_random_uuid(), '${householdA()}', 'Race probe ${cost}/${quantity}', ${cost}, true, ${quantity}, now(), now())
    RETURNING id;
  `).trim();
}

// postConcurrently fires n identical POSTs from the page's session at once,
// without awaiting between them, and returns their statuses. Sequential
// requests would never expose a check-then-write race.
async function postConcurrently(page, path, fields, n) {
  return page.evaluate(async ({ path, fields, n }) => {
    const body = new URLSearchParams(fields).toString();
    const once = () => fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body,
      redirect: 'manual',
    }).then((r) => (r.type === 'opaqueredirect' ? 303 : r.status));
    return Promise.all(Array.from({ length: n }, once));
  }, { path, fields, n });
}

test.describe('§6 rewards', () => {
  test('T-6.1.3 redeeming deducts the cost', async ({ page }) => {
    const child = memberId(PERSONAS.child.displayName);
    grantPoints(child, 100);
    const before = balance(child);
    const reward = seedReward({ cost: 10, quantity: 5 });

    await login(page, PERSONAS.child);
    await page.goto('/rewards');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
    const status = await postForm(page, `/rewards/${reward}/redeem`, { csrf_token });
    expect(status, 'a redemption within budget must be accepted').toBe(303);

    expect(balance(child), 'the cost must be deducted exactly once').toBe(before - 10);
  });

  test('T-6.2.1 redeeming beyond the balance is refused', async ({ page }) => {
    const child = memberId(PERSONAS.child.displayName);
    const reward = seedReward({ cost: 1_000_000, quantity: 5 });
    const before = balance(child);

    await login(page, PERSONAS.child);
    await page.goto('/rewards');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
    const status = await postForm(page, `/rewards/${reward}/redeem`, { csrf_token });

    expect(status, 'an unaffordable redemption must be refused').toBeGreaterThanOrEqual(400);
    expect(balance(child), 'a refused redemption must not move the balance').toBe(before);
  });

  test('T-0.7.5 [!] concurrent redemptions cannot overspend the balance', async ({ page }) => {
    const child = memberId(PERSONAS.child.displayName);
    // Exactly enough for ONE redemption, then five at once.
    const reward = seedReward({ cost: 50, quantity: 99 });
    const start = balance(child);
    grantPoints(child, 50 - start); // normalise to exactly 50
    expect(balance(child)).toBe(50);

    await login(page, PERSONAS.child);
    await page.goto('/rewards');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
    const statuses = await postConcurrently(page, `/rewards/${reward}/redeem`, { csrf_token }, 5);

    const accepted = statuses.filter((s) => s === 303).length;
    const after = balance(child);

    expect(after, 'the balance must never go negative').toBeGreaterThanOrEqual(0);
    expect(accepted, 'only one redemption was affordable').toBe(1);
    expect(after, 'exactly one cost may be deducted').toBe(0);
  });

  test('T-0.7.4 [!] concurrent redemptions cannot oversell the last unit', async ({ page }) => {
    const child = memberId(PERSONAS.child.displayName);
    grantPoints(child, 1000);
    const reward = seedReward({ cost: 5, quantity: 1 });

    await login(page, PERSONAS.child);
    await page.goto('/rewards');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
    const statuses = await postConcurrently(page, `/rewards/${reward}/redeem`, { csrf_token }, 5);

    const accepted = statuses.filter((s) => s === 303).length;
    const remaining = Number(psql(
      `SELECT quantity_available FROM nestova.reward WHERE id = '${reward}';`,
    ).trim());
    const redemptions = Number(psql(
      `SELECT count(*) FROM nestova.reward_redemption WHERE reward_id = '${reward}';`,
    ).trim());

    expect(accepted, 'only one unit was in stock').toBe(1);
    expect(redemptions, 'stock must not be oversold').toBe(1);
    expect(remaining, 'remaining stock must not go negative').toBeGreaterThanOrEqual(0);
  });

  test('T-6.2.3 an out-of-stock reward cannot be redeemed', async ({ page }) => {
    const child = memberId(PERSONAS.child.displayName);
    grantPoints(child, 1000);
    const reward = seedReward({ cost: 5, quantity: 0 });

    await login(page, PERSONAS.child);
    await page.goto('/rewards');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
    const status = await postForm(page, `/rewards/${reward}/redeem`, { csrf_token });
    expect(status, 'an out-of-stock reward must be refused').toBeGreaterThanOrEqual(400);
  });

  test('T-6.3.1/T-6.3.2 reward admin rejects an empty name and a non-positive cost', async ({ page }) => {
    await login(page, PERSONAS.owner);
    await page.goto('/admin/rewards/new');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();

    const emptyName = await postForm(page, '/admin/rewards', {
      csrf_token, name: '   ', cost_points: '10', quantity_available: '1',
    });
    const zeroCost = await postForm(page, '/admin/rewards', {
      csrf_token, name: 'Zero cost', cost_points: '0', quantity_available: '1',
    });
    const negativeCost = await postForm(page, '/admin/rewards', {
      csrf_token, name: 'Negative cost', cost_points: '-5', quantity_available: '1',
    });
    const negativeQty = await postForm(page, '/admin/rewards', {
      csrf_token, name: 'Negative qty', cost_points: '5', quantity_available: '-1',
    });

    const accepted = Object.entries({ emptyName, zeroCost, negativeCost, negativeQty })
      .filter(([, s]) => s < 400);
    expect(accepted, 'invalid reward payloads that were accepted').toEqual([]);
  });
});
