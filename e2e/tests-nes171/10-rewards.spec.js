// §6 rewards and points, and the §0.7 concurrency cases that live here.
//
// The concurrency tests are the point of this file. Redeeming spends a
// balance and decrements stock, so a race that is not serialised shows up as
// a negative balance or an oversold reward — the kind of defect that only
// appears under simultaneous requests, never in a sequential click-through.
//
// The §6 tests below the races each seed their own child (seedRewardsMember),
// so exact balances never depend on what an earlier spec did to the shared
// child persona.
const crypto = require('crypto');
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, postForm } = require('./helpers');
const { psql } = require('../tests/db');
const {
  memberId,
  seedRewardsMember,
  grantPoints,
  balance,
  lowestRunningBalance,
  seedReward,
  rewardRow,
  redemptions,
  redemptionStatus,
  ledgerFor,
  seedChoreInstance,
  rewardsToken,
  shownBalance,
  postConcurrently,
} = require('./helpers-rewards');

// The deep-link signing scheme, duplicated from the server (and from
// 09-deeplinks.spec.js) so T-6.4.2 can mint a real redeem link.
const DEEPLINK_SECRET = process.env.NESTOVA_SESSION_SECRET || 'dev-only-insecure-session-secret-change-me';

function signedDeepLink(path, secondsFromNow = 300) {
  const key = crypto.createHmac('sha256', DEEPLINK_SECRET).update('nestova:deeplink:v1').digest();
  const exp = Math.floor(Date.now() / 1000) + secondsFromNow;
  const sig = crypto.createHmac('sha256', key).update(`${path}|${exp}`).digest().toString('base64url');
  return `${path}?exp=${exp}&sig=${encodeURIComponent(sig)}`;
}

// redeemAs redeems rewardId as the signed-in member and returns the status.
async function redeem(page, rewardId) {
  const csrf_token = await rewardsToken(page);
  return postForm(page, `/rewards/${rewardId}/redeem`, { csrf_token });
}

async function adminToken(page) {
  await page.goto('/admin/rewards');
  return page.locator('input[name="csrf_token"]').first().inputValue();
}

async function resolveRedemption(page, redemptionId, verb, fields = {}) {
  const csrf_token = await adminToken(page);
  return postForm(page, `/admin/rewards/redemptions/${redemptionId}/${verb}`, { csrf_token, ...fields });
}

async function cancelRedemption(page, redemptionId) {
  const csrf_token = await rewardsToken(page);
  return postForm(page, `/rewards/redemptions/${redemptionId}/cancel`, { csrf_token });
}

// storefrontNames lists the reward names the signed-in member is offered.
async function storefrontNames(page) {
  await page.goto('/rewards');
  await expect(page.locator('h2', { hasText: 'Your Balance' })).toBeVisible();
  return page.locator('h2:has-text("Rewards") + ul li p.font-medium').allInnerTexts();
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

    expect(status, 'an unaffordable redemption must be refused with 409').toBe(409);
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

  // Kept apart from T-6.2.3 because test.fail accepts any failure: a broken
  // redemption inside that test would be reported as the expected defect.
  test('sanity: an in-stock reward is redeemable and debits the balance', async ({ page }) => {
    const { id: kid, persona } = seedRewardsMember('InStock');
    grantPoints(kid, 100);
    const inStock = seedReward({ cost: 5, quantity: 1 });

    await login(page, persona);
    expect(await redeem(page, inStock)).toBe(303);
    expect(balance(kid)).toBe(95);
  });

  test('T-6.2.3 an out-of-stock reward is refused with 409, not 500', async ({ page }) => {
    test.fail(true, 'DEFECT: Redeem maps ErrRewardOutOfStock to 500 instead of a 409 conflict');
    const { id: kid, persona } = seedRewardsMember('Stock');
    grantPoints(kid, 100);
    const soldOut = seedReward({ cost: 5, quantity: 0 });

    await login(page, persona);
    const status = await redeem(page, soldOut);
    expect(redemptions(kid, soldOut), 'no redemption may be recorded').toEqual([]);
    expect(balance(kid), 'a refused redemption must not move the balance').toBe(100);
    expect(status, 'out of stock is a conflict, and any 500 is a failure').toBe(409);
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

  // -------------------------------------------------------------------------
  // 6.1 earning and spending
  // -------------------------------------------------------------------------

  test('T-6.1.1 completing a chore raises the balance and writes a ledger entry', async ({ page }) => {
    const { id: kid, persona } = seedRewardsMember('Earn');
    const title = `Rewards earn ${Date.now()}`;
    const instance = seedChoreInstance({ assignee: kid, points: 7, title });

    await login(page, persona);
    expect(await shownBalance(page), 'a new member starts at zero').toBe(0);

    await page.goto('/tasks');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
    expect(await postForm(page, `/tasks/${instance}/complete`, { csrf_token }), 'the completion must be accepted').toBe(303);

    expect(ledgerFor(instance), 'exactly one task award is recorded')
      .toEqual([{ sourceType: 'task_instance', points: 7 }]);
    expect(await shownBalance(page), 'the balance card reflects the award').toBe(7);
    await expect(page.getByText(`Completed: ${title}`), 'the award appears in Recent Activity').toBeVisible();
  });

  test('T-6.1.2 /rewards renders for a child', async ({ page }) => {
    await login(page, PERSONAS.child);
    const res = await page.goto('/rewards');

    expect(res.status()).toBe(200);
    await expect(page.locator('h2', { hasText: 'Your Balance' })).toBeVisible();
    await expect(page.locator('h2', { hasText: 'Leaderboard' })).toBeVisible();
    await expect(page.locator('h2', { hasText: 'Rewards' }).first()).toBeVisible();
    await expect(page.getByRole('link', { name: 'Manage rewards' }), 'a child is not offered reward admin').toHaveCount(0);
  });

  test('T-6.1.4 cancelling your own redemption refunds the points exactly', async ({ page }) => {
    const { id: kid, persona } = seedRewardsMember('Cancel');
    grantPoints(kid, 30);
    const reward = seedReward({ cost: 12, quantity: 5 });

    await login(page, persona);
    expect(await redeem(page, reward)).toBe(303);
    expect(balance(kid)).toBe(18);
    const [redemption] = redemptions(kid, reward);

    // Through the UI: the Cancel button on the member's own pending row.
    await page.goto('/rewards');
    await page.locator(`#my-redemption-${redemption.id}`).getByRole('button', { name: 'Cancel' }).click();
    await expect(page.locator(`#my-redemption-${redemption.id}`)).toContainText('Cancelled');

    expect(redemptionStatus(redemption.id)).toBe('cancelled');
    expect(ledgerFor(redemption.id), 'the refund mirrors the debit exactly').toEqual([
      { sourceType: 'redemption', points: -12 },
      { sourceType: 'redemption_refund', points: 12 },
    ]);
    expect(balance(kid)).toBe(30);
    expect(await shownBalance(page)).toBe(30);
  });

  // -------------------------------------------------------------------------
  // 6.2 redemption guards
  // -------------------------------------------------------------------------

  test('T-6.2.2 redeeming with exactly enough points succeeds and leaves zero', async ({ page }) => {
    const { id: kid, persona } = seedRewardsMember('Exact');
    grantPoints(kid, 25);
    const reward = seedReward({ cost: 25, quantity: 5 });

    await login(page, persona);
    expect(await redeem(page, reward), 'an exactly-affordable reward is redeemable').toBe(303);
    expect(redemptions(kid, reward).map((r) => r.status)).toEqual(['pending']);
    expect(balance(kid)).toBe(0);
    expect(await shownBalance(page)).toBe(0);
  });

  test('T-6.2.4 an archived reward is not redeemable', async ({ page, browser }) => {
    const { id: kid, persona } = seedRewardsMember('Archived');
    grantPoints(kid, 100);
    const control = seedReward({ cost: 5 });
    const retired = seedReward({ cost: 5 });

    const ownerContext = await browser.newContext();
    const owner = await ownerContext.newPage();
    await login(owner, PERSONAS.owner);
    const csrf_token = await adminToken(owner);
    expect(await postForm(owner, `/admin/rewards/${retired}/archive`, { csrf_token })).toBe(303);
    expect(rewardRow(retired).active).toBe(false);
    await ownerContext.close();

    await login(page, persona);
    const offered = await storefrontNames(page);
    expect(offered, 'the active control reward is offered').toContain(rewardRow(control).name);
    expect(offered, 'the archived reward is not offered').not.toContain(rewardRow(retired).name);

    expect(await redeem(page, control), 'sanity: the same route accepts an active reward').toBe(303);
    expect(await redeem(page, retired), 'an archived reward is treated as not found').toBe(404);
    expect(redemptions(kid, retired)).toEqual([]);
    expect(balance(kid)).toBe(95);
  });

  test('T-6.2.5 cancelling an already-fulfilled redemption is refused', async ({ page, browser }) => {
    const { id: kid, persona } = seedRewardsMember('Fulfilled');
    grantPoints(kid, 50);
    const reward = seedReward({ cost: 10, quantity: 5 });

    await login(page, persona);
    expect(await redeem(page, reward)).toBe(303);
    expect(await redeem(page, reward)).toBe(303);
    const [fulfilled, pending] = redemptions(kid, reward);

    const ownerContext = await browser.newContext();
    const owner = await ownerContext.newPage();
    await login(owner, PERSONAS.owner);
    expect(await resolveRedemption(owner, fulfilled.id, 'fulfill')).toBe(303);
    await ownerContext.close();

    expect(await cancelRedemption(page, pending.id), 'sanity: a pending redemption can be cancelled').toBe(303);
    expect(await cancelRedemption(page, fulfilled.id), 'ErrRedemptionNotPending maps to 409').toBe(409);
    expect(redemptionStatus(fulfilled.id)).toBe('fulfilled');
    expect(ledgerFor(fulfilled.id), 'no refund for a fulfilled redemption')
      .toEqual([{ sourceType: 'redemption', points: -10 }]);
    expect(balance(kid)).toBe(40);
  });

  test('T-6.2.6 deny refunds the points; fulfil does not', async ({ page, browser }) => {
    const { id: kid, persona } = seedRewardsMember('Resolve');
    grantPoints(kid, 40);
    const reward = seedReward({ cost: 15, quantity: 5 });

    await login(page, persona);
    expect(await redeem(page, reward)).toBe(303);
    expect(await redeem(page, reward)).toBe(303);
    expect(balance(kid)).toBe(10);
    const [toDeny, toFulfil] = redemptions(kid, reward);

    const ownerContext = await browser.newContext();
    const owner = await ownerContext.newPage();
    await login(owner, PERSONAS.owner);
    expect(await resolveRedemption(owner, toDeny.id, 'deny', { reason: 'Not today' })).toBe(303);
    expect(await resolveRedemption(owner, toFulfil.id, 'fulfill')).toBe(303);
    // A resolved redemption cannot be resolved a second time.
    expect(await resolveRedemption(owner, toDeny.id, 'fulfill')).toBe(409);
    expect(await resolveRedemption(owner, toFulfil.id, 'deny')).toBe(409);
    await ownerContext.close();

    expect(redemptionStatus(toDeny.id)).toBe('denied');
    expect(redemptionStatus(toFulfil.id)).toBe('fulfilled');
    expect(ledgerFor(toDeny.id), 'deny refunds the exact debit').toEqual([
      { sourceType: 'redemption', points: -15 },
      { sourceType: 'redemption_refund', points: 15 },
    ]);
    expect(ledgerFor(toFulfil.id), 'fulfil keeps the debit').toEqual([
      { sourceType: 'redemption', points: -15 },
    ]);
    expect(balance(kid)).toBe(25);
    expect(await shownBalance(page)).toBe(25);
    await expect(page.locator(`#my-redemption-${toDeny.id}`)).toContainText('Not today');
  });

  test("T-6.2.7 cancelling someone else's redemption is refused", async ({ page, browser }) => {
    const { id: owner, persona: ownerPersona } = seedRewardsMember('Victim');
    const { id: other, persona: otherPersona } = seedRewardsMember('Intruder');
    grantPoints(owner, 30);
    grantPoints(other, 30);
    const reward = seedReward({ cost: 10, quantity: 5 });

    await login(page, ownerPersona);
    expect(await redeem(page, reward)).toBe(303);
    expect(await redeem(page, reward)).toBe(303);
    const [target, own] = redemptions(owner, reward);

    const otherContext = await browser.newContext();
    const intruder = await otherContext.newPage();
    await login(intruder, otherPersona);
    expect(await cancelRedemption(intruder, target.id), "another member's redemption cannot be cancelled").toBe(409);
    await otherContext.close();

    // A parent may resolve a redemption through the admin inbox, but the
    // member-facing cancel is the redeemer's alone.
    const parentContext = await browser.newContext();
    const parent = await parentContext.newPage();
    await login(parent, PERSONAS.owner);
    expect(await cancelRedemption(parent, target.id), "a parent cannot use the member's cancel either").toBe(409);
    await parentContext.close();

    expect(redemptionStatus(target.id)).toBe('pending');
    expect(balance(other), "the intruder must not receive the refund").toBe(30);
    expect(balance(owner)).toBe(10);

    expect(await cancelRedemption(page, own.id), 'sanity: the redeemer can cancel their own').toBe(303);
    expect(balance(owner)).toBe(20);
  });

  // -------------------------------------------------------------------------
  // 6.3 reward admin
  // -------------------------------------------------------------------------

  test('T-6.3.3 quantity 0 is accepted and kept off the storefront; negative is refused', async ({ page, browser }) => {
    const name = `Zero stock ${Date.now()}`;
    await login(page, PERSONAS.owner);
    await page.goto('/admin/rewards/new');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();

    const negative = await postForm(page, '/admin/rewards', {
      csrf_token, name: `${name} neg`, cost_points: '5', quantity_available: '-1',
    });
    expect(negative, 'a negative quantity is refused').toBe(422);
    expect(psql(`SELECT count(*) FROM nestova.reward WHERE name = '${name} neg';`).trim()).toBe('0');

    const zero = await postForm(page, '/admin/rewards', {
      csrf_token, name, cost_points: '5', quantity_available: '0',
    });
    expect(zero, 'a quantity of zero is a valid, sold-out reward').toBe(303);
    const id = psql(`SELECT id FROM nestova.reward WHERE name = '${name}';`).trim();
    expect(rewardRow(id)).toMatchObject({ active: true, quantity: 0 });

    await page.goto('/admin/rewards');
    await expect(page.locator('li', { hasText: name }).first()).toContainText('0 available');

    const { id: kid, persona } = seedRewardsMember('Zero');
    grantPoints(kid, 100);
    const memberContext = await browser.newContext();
    const member = await memberContext.newPage();
    await login(member, persona);
    expect(await storefrontNames(member), 'a sold-out reward is not offered').not.toContain(name);
    await memberContext.close();
  });

  test('T-6.3.3 a quantity-0 reward is refused at redeem with 409, not 500', async ({ page }) => {
    test.fail(true, 'DEFECT: Redeem maps ErrRewardOutOfStock to 500 instead of a 409 conflict');
    const { id: kid, persona } = seedRewardsMember('ZeroRedeem');
    grantPoints(kid, 100);
    const reward = seedReward({ cost: 5, quantity: 0 });

    await login(page, persona);
    const status = await redeem(page, reward);
    expect(redemptions(kid, reward), 'a quantity-0 reward is not redeemable').toEqual([]);
    expect(balance(kid)).toBe(100);
    expect(status, 'any 500 is a failure').toBe(409);
  });

  test('sanity: the reward form accepts a valid cost', async ({ page }) => {
    const name = `Valid cost ${Date.now()}`;
    await login(page, PERSONAS.owner);
    await page.goto('/admin/rewards/new');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();

    const valid = await postForm(page, '/admin/rewards', {
      csrf_token, name, cost_points: '10', quantity_available: '',
    });
    expect(valid, 'a valid reward is accepted').toBe(303);
    expect(psql(`SELECT count(*) FROM nestova.reward WHERE name = '${name}';`).trim()).toBe('1');
  });

  test('T-6.3.4 [!] a MaxInt64 cost is refused with 422, not 500', async ({ page }) => {
    const name = `Huge cost ${Date.now()}`;
    await login(page, PERSONAS.owner);
    await page.goto('/admin/rewards/new');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();

    const huge = await postForm(page, '/admin/rewards', {
      csrf_token, name, cost_points: '9223372036854775807', quantity_available: '',
    });
    expect(psql(`SELECT count(*) FROM nestova.reward WHERE name = '${name}';`).trim()).toBe('0');
    expect(huge, 'an unstorable cost must be a validation error').toBe(422);
  });

  test('T-6.3.4 [!] the largest storable cost cannot overflow a balance', async ({ page }) => {
    const { id: kid, persona } = seedRewardsMember('Overflow');
    // A ledger row is int4, so the richest single grant is MaxInt32; the
    // balance SUM is bigint and must not wrap.
    grantPoints(kid, 2147483647);
    grantPoints(kid, 2147483647);
    const max = seedReward({ cost: 2147483647 });
    const cheap = seedReward({ cost: 1 });

    await login(page, persona);
    expect(await shownBalance(page), 'the balance must not wrap past MaxInt32').toBe(4294967294);
    expect(await redeem(page, max), 'a MaxInt32-cost reward is redeemable by a rich member').toBe(303);
    expect(await redeem(page, max)).toBe(303);
    expect(balance(kid)).toBe(0);
    expect(await redeem(page, max), 'and refused once the balance is spent').toBe(409);
    expect(await redeem(page, cheap)).toBe(409);
    expect(lowestRunningBalance(kid)).toBeGreaterThanOrEqual(0);
  });

  test('T-6.3.5 archiving a reward with pending redemptions keeps them resolvable', async ({ page, browser }) => {
    const { id: kid, persona } = seedRewardsMember('ArchivePending');
    grantPoints(kid, 40);
    const reward = seedReward({ cost: 10, quantity: 5 });
    const { name } = rewardRow(reward);

    await login(page, persona);
    expect(await redeem(page, reward)).toBe(303);
    expect(await redeem(page, reward)).toBe(303);
    const [toDeny, toFulfil] = redemptions(kid, reward);

    const ownerContext = await browser.newContext();
    const owner = await ownerContext.newPage();
    await login(owner, PERSONAS.owner);
    const csrf_token = await adminToken(owner);
    expect(await postForm(owner, `/admin/rewards/${reward}/archive`, { csrf_token }), 'archive is allowed with pending redemptions').toBe(303);
    expect(rewardRow(reward).active).toBe(false);

    // The pending redemptions survive the archive and stay in the inbox.
    await owner.goto('/admin/rewards');
    await expect(owner.locator('#redemption-inbox')).toContainText(name);
    expect(await resolveRedemption(owner, toDeny.id, 'deny')).toBe(303);
    expect(await resolveRedemption(owner, toFulfil.id, 'fulfill')).toBe(303);
    await ownerContext.close();

    expect(redemptionStatus(toDeny.id)).toBe('denied');
    expect(redemptionStatus(toFulfil.id)).toBe('fulfilled');
    expect(balance(kid), 'the denied redemption is refunded, the fulfilled one is not').toBe(30);
    // The member's history still names the archived reward.
    await page.goto('/rewards');
    await expect(page.locator(`#my-redemption-${toFulfil.id}`)).toContainText(name);
    expect(await storefrontNames(page)).not.toContain(name);
  });

  test('T-6.3.6 a price change while pending does not alter the pending redemption', async ({ page, browser }) => {
    const { id: kid, persona } = seedRewardsMember('Reprice');
    grantPoints(kid, 100);
    const reward = seedReward({ cost: 10, quantity: 5 });
    const { name } = rewardRow(reward);

    await login(page, persona);
    expect(await redeem(page, reward)).toBe(303);
    expect(await redeem(page, reward)).toBe(303);
    const [toCancel, toDeny] = redemptions(kid, reward);
    expect(balance(kid)).toBe(80);

    const ownerContext = await browser.newContext();
    const owner = await ownerContext.newPage();
    await login(owner, PERSONAS.owner);
    await owner.goto(`/admin/rewards/${reward}/edit`);
    const csrf_token = await owner.locator('input[name="csrf_token"]').first().inputValue();
    expect(await postForm(owner, `/admin/rewards/${reward}`, {
      csrf_token, name, description: '', image_ref: '', cost_points: '50', quantity_available: '5',
    }), 'the reprice is accepted').toBe(303);
    expect(rewardRow(reward).cost).toBe(50);
    expect(await resolveRedemption(owner, toDeny.id, 'deny')).toBe(303);
    await ownerContext.close();

    expect(await cancelRedemption(page, toCancel.id)).toBe(303);
    expect(ledgerFor(toCancel.id), 'cancel refunds the original 10, not the new 50').toEqual([
      { sourceType: 'redemption', points: -10 },
      { sourceType: 'redemption_refund', points: 10 },
    ]);
    expect(ledgerFor(toDeny.id), 'deny refunds the original 10, not the new 50').toEqual([
      { sourceType: 'redemption', points: -10 },
      { sourceType: 'redemption_refund', points: 10 },
    ]);
    expect(balance(kid)).toBe(100);

    // A new redemption is charged the new price.
    expect(await redeem(page, reward)).toBe(303);
    expect(balance(kid)).toBe(50);
  });

  // -------------------------------------------------------------------------
  // 6.4 abuse and consistency
  // -------------------------------------------------------------------------

  test('T-6.4.1 a child is refused every /admin/rewards route, GET and POST', async ({ page, browser }) => {
    const { id: kid, persona } = seedRewardsMember('Admin');
    grantPoints(kid, 20);
    const reward = seedReward({ cost: 5, quantity: 5 });
    const before = rewardRow(reward);

    // Sanity: an owner is served the same routes.
    const ownerContext = await browser.newContext();
    const owner = await ownerContext.newPage();
    await login(owner, PERSONAS.owner);
    for (const path of ['/admin/rewards', '/admin/rewards/new', `/admin/rewards/${reward}/edit`]) {
      expect((await owner.request.get(path, { maxRedirects: 0 })).status(), `owner GET ${path}`).toBe(200);
    }
    await ownerContext.close();

    await login(page, persona);
    expect(await redeem(page, reward)).toBe(303);
    const [redemption] = redemptions(kid, reward);
    const csrf_token = await rewardsToken(page);

    const gets = {};
    for (const path of ['/admin/rewards', '/admin/rewards/new', `/admin/rewards/${reward}/edit`]) {
      gets[path] = (await page.request.get(path, { maxRedirects: 0 })).status();
    }
    const posts = {
      create: await postForm(page, '/admin/rewards', {
        csrf_token, name: `Child made ${Date.now()}`, cost_points: '1', quantity_available: '',
      }),
      update: await postForm(page, `/admin/rewards/${reward}`, {
        csrf_token, name: 'Hijacked', cost_points: '1', quantity_available: '99',
      }),
      archive: await postForm(page, `/admin/rewards/${reward}/archive`, { csrf_token }),
      fulfil: await postForm(page, `/admin/rewards/redemptions/${redemption.id}/fulfill`, { csrf_token }),
      deny: await postForm(page, `/admin/rewards/redemptions/${redemption.id}/deny`, { csrf_token }),
    };

    expect(gets).toEqual(Object.fromEntries(Object.keys(gets).map((k) => [k, 403])));
    expect(posts).toEqual(Object.fromEntries(Object.keys(posts).map((k) => [k, 403])));
    expect(rewardRow(reward), 'the reward is untouched').toEqual(before);
    expect(redemptionStatus(redemption.id), 'the child cannot resolve their own redemption').toBe('pending');
    expect(psql("SELECT count(*) FROM nestova.reward WHERE name LIKE 'Child made %';").trim()).toBe('0');
  });

  test('T-6.4.2 replaying a redeem deep link is refused with ErrDeepLinkAlreadyRedeemed', async ({ page }) => {
    const { id: kid, persona } = seedRewardsMember('DeepLink');
    grantPoints(kid, 50);
    const reward = seedReward({ cost: 10, quantity: 5 });
    const link = signedDeepLink(`/go/redeem-reward/${reward}`);

    await login(page, persona);
    await page.goto(link);
    await page.getByRole('button', { name: 'Redeem' }).click();
    await page.waitForURL((u) => new URL(u).pathname === '/go/redeem-reward/done');
    expect(balance(kid), 'the first use of the link redeems').toBe(40);

    // Back to the confirm screen and submit the same signed link again.
    await page.goBack();
    await page.getByRole('button', { name: 'Redeem' }).click();
    await expect(page.getByText('This code has already been used')).toBeVisible();

    // And a raw replay of the POST, as a resubmitted request would send it.
    await page.goto(link);
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
    expect(await postForm(page, link, { csrf_token })).toBe(409);

    expect(redemptions(kid, reward).length, 'one link, one redemption').toBe(1);
    expect(balance(kid), 'points are debited once').toBe(40);
  });

  test('T-6.4.3 redeem, cancel, redeem again keeps balance and stock consistent', async ({ page }) => {
    const { id: kid, persona } = seedRewardsMember('Cycle');
    grantPoints(kid, 40);
    // One unit: the cancel must free it for the second redemption.
    const reward = seedReward({ cost: 15, quantity: 1 });

    await login(page, persona);
    expect(await redeem(page, reward)).toBe(303);
    expect(balance(kid)).toBe(25);
    const [first] = redemptions(kid, reward);
    expect(await cancelRedemption(page, first.id)).toBe(303);
    expect(balance(kid)).toBe(40);
    expect(await redeem(page, reward), 'the cancelled unit is back in stock').toBe(303);
    expect(balance(kid)).toBe(25);

    const [, second] = redemptions(kid, reward);
    expect(redemptions(kid, reward).map((r) => r.status)).toEqual(['cancelled', 'pending']);
    expect(ledgerFor(first.id)).toEqual([
      { sourceType: 'redemption', points: -15 },
      { sourceType: 'redemption_refund', points: 15 },
    ]);
    expect(ledgerFor(second.id)).toEqual([{ sourceType: 'redemption', points: -15 }]);
    expect(await shownBalance(page)).toBe(25);
    // The single unit is taken again. Redeeming it a third time would hit the
    // out-of-stock 500 that T-6.2.3 records, so the storefront is checked.
    expect(await storefrontNames(page), 'the unit is spoken for again').not.toContain(rewardRow(reward).name);
  });

  test('T-6.4.4 the balance never goes negative across a mixed sequence', async ({ page, browser }) => {
    const { id: kid, persona } = seedRewardsMember('Never');
    grantPoints(kid, 20);
    const big = seedReward({ cost: 15 });
    const all = seedReward({ cost: 20 });
    const ten = seedReward({ cost: 10 });

    const ownerContext = await browser.newContext();
    const owner = await ownerContext.newPage();
    await login(owner, PERSONAS.owner);
    await login(page, persona);

    expect(await redeem(page, big)).toBe(303);                  // 20 -> 5
    expect(await redeem(page, big), 'unaffordable').toBe(409);  // stays 5
    const [denied] = redemptions(kid, big);
    expect(await resolveRedemption(owner, denied.id, 'deny')).toBe(303); // -> 20
    expect(await redeem(page, all)).toBe(303);                  // -> 0
    expect(await redeem(page, ten), 'nothing left').toBe(409);
    const [fulfilled] = redemptions(kid, all);
    expect(await resolveRedemption(owner, fulfilled.id, 'fulfill')).toBe(303);
    expect(await cancelRedemption(page, fulfilled.id), 'no refund after fulfil').toBe(409);
    expect(await resolveRedemption(owner, denied.id, 'deny'), 'no second refund').toBe(409);
    expect(balance(kid)).toBe(0);

    // Refill to 20 and race five redemptions of 10: exactly two fit.
    grantPoints(kid, 20);
    const csrf_token = await rewardsToken(page);
    const statuses = await postConcurrently(page, `/rewards/${ten}/redeem`, { csrf_token }, 5);
    await ownerContext.close();

    expect(statuses.filter((s) => s === 303).length).toBe(2);
    expect(statuses.filter((s) => s !== 303 && s !== 409), 'every refusal is a clean 409').toEqual([]);
    expect(balance(kid)).toBe(0);
    expect(lowestRunningBalance(kid), 'no intermediate step went below zero').toBeGreaterThanOrEqual(0);
    expect(await shownBalance(page)).toBe(0);
  });
});
