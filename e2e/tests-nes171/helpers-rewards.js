// Seeding and inspection helpers for the §6 rewards specs.
//
// A balance is the plain SUM of a member's point_ledger rows (see
// PointLedgerPostgresRepository.Balance), so a ledger row is all it takes to
// fund a member. Earning points through a chore completion is exercised by
// T-6.1.1 itself; every other test funds its member directly.
const { expect } = require('@playwright/test');
const { PERSONAS, PASSWORD } = require('../tests/fixtures');
const { psql, seedMemberInA } = require('../tests/db');

function householdA() {
  return psql("SELECT id FROM identity.household WHERE name = 'Household A' LIMIT 1;").trim();
}

function memberId(displayName) {
  return psql(`SELECT id FROM identity.member WHERE display_name = '${displayName}' LIMIT 1;`).trim();
}

// seedRewardsMember adds a brand-new child to household A and returns its id
// and a persona that can sign in. A fresh member starts at a zero balance with
// no redemptions, so a test can assert exact balances without depending on
// what earlier specs did to the shared personas.
function seedRewardsMember(label) {
  const suffix = `${Date.now()}-${Math.floor(Math.random() * 1e6)}`;
  const displayName = `Rewards ${label} ${suffix}`;
  const email = `rewards-${label.toLowerCase()}-${suffix}@test.local`;
  const id = seedMemberInA({ displayName, email, role: 'child', copyHashFrom: PERSONAS.child.email });
  return { id, persona: { email, password: PASSWORD, displayName, role: 'child' } };
}

// grantPoints writes an 'adjustment' ledger row, the same shape a manual
// point adjustment would have.
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

// lowestRunningBalance replays the member's ledger in insertion order and
// returns the lowest balance it ever held, so "never negative" is checked at
// every step rather than only at the end.
function lowestRunningBalance(member) {
  return Number(psql(`
    SELECT coalesce(min(running), 0) FROM (
      SELECT sum(points) OVER (ORDER BY created_at, id) AS running
        FROM nestova.point_ledger WHERE member_id = '${member}'
    ) steps;`).trim());
}

function seedReward({ cost, quantity = null, active = true, name }) {
  const rewardName = name || `Reward probe ${Date.now()}-${Math.floor(Math.random() * 1e6)}`;
  const qty = quantity === null ? 'NULL' : quantity;
  return psql(`
    INSERT INTO nestova.reward
      (id, household_id, name, cost_points, active, quantity_available, created_at, updated_at)
    VALUES (gen_random_uuid(), '${householdA()}', '${rewardName}', ${cost}, ${active}, ${qty}, now(), now())
    RETURNING id;
  `).trim();
}

function rewardRow(rewardId) {
  const [name, cost, active, quantity] = psql(
    `SELECT name, cost_points, active, coalesce(quantity_available::text, '') FROM nestova.reward WHERE id = '${rewardId}';`,
  ).trim().split('|');
  return { name, cost: Number(cost), active: active === 't', quantity: quantity === '' ? null : Number(quantity) };
}

// redemptions lists a member's redemptions of one reward, oldest first.
function redemptions(member, rewardId) {
  const out = psql(`
    SELECT id || '|' || status FROM nestova.reward_redemption
     WHERE member_id = '${member}' AND reward_id = '${rewardId}'
     ORDER BY created_at, id;`).trim();
  return out ? out.split('\n').map((line) => {
    const [id, status] = line.split('|');
    return { id, status };
  }) : [];
}

function redemptionStatus(redemptionId) {
  return psql(`SELECT status FROM nestova.reward_redemption WHERE id = '${redemptionId}';`).trim();
}

// ledgerFor returns the signed point amounts recorded against a source row
// (a redemption's debit and any refund), oldest first.
function ledgerFor(sourceId) {
  const out = psql(`
    SELECT source_type || '|' || points FROM nestova.point_ledger
     WHERE source_id = '${sourceId}' ORDER BY created_at, id;`).trim();
  return out ? out.split('\n').map((line) => {
    const [sourceType, points] = line.split('|');
    return { sourceType, points: Number(points) };
  }) : [];
}

// seedChoreInstance creates a fixed-assignee chore worth `points` and the
// pending instance the background scheduler would otherwise materialise
// minutes later.
function seedChoreInstance({ assignee, points, title }) {
  const household = householdA();
  const taskId = psql(`
    INSERT INTO nestova.recurring_task
      (id, household_id, title, category, cadence, rotation_policy, points, lead_time_days, active, created_at, updated_at)
    VALUES (gen_random_uuid(), '${household}', '${title}', 'chore',
            '{"Freq":"daily","Interval":1,"Anchor":"2026-08-25T00:00:00Z","ByWeekday":null}'::jsonb,
            'fixed', ${points}, 0, true, now(), now())
    RETURNING id;`).trim();
  return psql(`
    INSERT INTO nestova.task_instance
      (id, recurring_task_id, household_id, assignee_id, due_on, status, kind, created_at, updated_at)
    VALUES (gen_random_uuid(), '${taskId}', '${household}', '${assignee}', current_date, 'pending', 'scheduled', now(), now())
    RETURNING id;`).trim();
}

async function rewardsToken(page) {
  await page.goto('/rewards');
  return page.locator('input[name="csrf_token"]').first().inputValue();
}

// shownBalance reads the "Your Balance" card on /rewards. It waits for the
// card to be visible first: <main> fades in after load, and innerText of a
// not-yet-rendered element is empty.
async function shownBalance(page) {
  await page.goto('/rewards');
  const card = page.locator('h2:has-text("Your Balance") + p');
  await expect(card).toBeVisible();
  const text = await card.innerText();
  return Number(text.replace(/[^0-9-]/g, ''));
}

// postConcurrently fires n identical POSTs from the page's session at once,
// without awaiting between them, and returns their statuses.
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

module.exports = {
  householdA,
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
};
