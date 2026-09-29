// §5.5 chore trades (NES-121/NES-122): accepting reassigns both chores, only
// the recipient may accept, terminal and claimed chores cannot trade, one chore
// can be in at most one live trade, and points follow the chore to its new
// assignee. T-5.5.1/2/4/5 already live in 11-chores.spec.js.
//
// Instances are seeded with SQL (the scheduler takes minutes to materialise
// them); every proposal, accept, decline and completion goes through the real
// routes. The shared personas have no PIN enrolled; the one test that needs
// the NES-166 PIN gate seeds its own members.
//
// Trade chores are due TRADE_DUE_IN_DAYS out, not today. A trade expires at
// the earlier chore's due_on, stored as that date's 00:00 UTC, so a trade on
// a chore due today is already expired when it is proposed and can never be
// accepted (the T-5.5.3 defect below). Seeding two days out keeps the other
// tests about what they test rather than tripping over that.
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { psql, seedMemberInA } = require('../tests/db');
const { login, postForm } = require('./helpers');
const { memberId, seedInstance, instanceRow, tasksToken } = require('./helpers-chore-photos');

const TRADE_DUE_IN_DAYS = 2;

// seedTradeChore seeds a chore that can be traded and accepted: see
// TRADE_DUE_IN_DAYS.
function seedTradeChore(assignee, titlePrefix, extra = {}) {
  return seedInstance({ assignee, titlePrefix, dueInDays: TRADE_DUE_IN_DAYS, ...extra });
}

const ids = () => ({
  owner: memberId(PERSONAS.owner.displayName),
  adult: memberId(PERSONAS.adult.displayName),
  child: memberId(PERSONAS.child.displayName),
});

function tradeFor(offeredInstanceId) {
  const out = psql(
    `SELECT id || '|' || status FROM nestova.chore_trade
      WHERE offered_instance_id = '${offeredInstanceId}' ORDER BY created_at DESC LIMIT 1;`,
  ).trim();
  if (!out) return null;
  const [id, status] = out.split('|');
  return { id, status };
}

function liveTradesTouching(instanceId) {
  return Number(psql(
    `SELECT count(*) FROM nestova.chore_trade
      WHERE status = 'proposed'
        AND (offered_instance_id = '${instanceId}' OR requested_instance_id = '${instanceId}');`,
  ).trim());
}

// session opens a fresh browser context signed in as persona and returns its
// page plus a /tasks CSRF token.
async function session(browser, persona) {
  const context = await browser.newContext();
  const page = await context.newPage();
  await login(page, persona);
  const csrf_token = await tasksToken(page);
  return { context, page, csrf_token };
}

function propose(s, offered, requested) {
  return postForm(s.page, '/trades', {
    csrf_token: s.csrf_token, offered_instance_id: offered, requested_instance_id: requested,
  });
}

// proposeThenAcceptFromDashboard drives the whole UI flow: the owner proposes
// through the picker page, then the adult clicks Accept on the dashboard card.
// Returns the accept response's status, the card locator and the adult's
// session.
async function proposeThenAcceptFromDashboard(browser, owner, offered, requested) {
  await owner.page.goto(`/tasks/${offered.instanceId}/propose-trade`);
  await expect(owner.page.getByRole('heading', { name: 'Propose a chore trade' })).toBeVisible();
  await owner.page.locator(`input[name="requested_instance_id"][value="${requested.instanceId}"]`).check();
  await owner.page.getByRole('button', { name: 'Propose trade' }).click();
  await owner.page.waitForURL((u) => new URL(u).pathname === '/');
  const trade = tradeFor(offered.instanceId);
  expect(trade?.status, 'the proposal is recorded').toBe('proposed');

  const adult = await session(browser, PERSONAS.adult);
  await adult.page.goto('/');
  const card = adult.page.locator(`#trade-${trade.id}`);
  await expect(card).toContainText(offered.title);
  await expect(card).toContainText(requested.title);
  const accepted = adult.page.waitForResponse(
    (r) => r.request().method() === 'POST' && r.url().endsWith(`/trades/${trade.id}/accept`),
  );
  await card.getByRole('button', { name: 'Accept' }).click();
  return { acceptStatus: (await accepted).status(), card, adult };
}

test.describe('§5.5 trades', () => {
  test('T-5.5.3 accepting a trade on chores due today swaps them', async ({ browser }) => {
    test.fail(true, 'DEFECT: a trade on a chore due today expires at that date 00:00 UTC, before it is even proposed, so Accept always returns 409');
    // /tasks offers the Trade link on today's chores and Propose accepts the
    // proposal, but chore_trade.expires_at is the earlier due_on at 00:00 UTC —
    // already past — so Accept's `expires_at > now` guard refuses it with
    // ErrTradeNotPending ("trade is no longer pending") every time.
    const m = ids();
    const offered = seedInstance({ assignee: m.owner, titlePrefix: 'Trade today' });
    const requested = seedInstance({ assignee: m.adult, titlePrefix: 'Trade today req' });

    const owner = await session(browser, PERSONAS.owner);
    await owner.page.goto('/tasks');
    await expect(owner.page.locator(`#task-${offered.instanceId}`).getByRole('link', { name: 'Trade' })).toBeVisible();
    const flow = await proposeThenAcceptFromDashboard(browser, owner, offered, requested);

    expect(flow.acceptStatus, 'the recipient must be able to accept a trade the app let them receive').toBe(200);
    expect(instanceRow(offered.instanceId).assignee).toBe(m.adult);
    expect(instanceRow(requested.instanceId).assignee).toBe(m.owner);

    await owner.context.close();
    await flow.adult.context.close();
  });

  test('trade accept control: chores due in two days swap on accept from the dashboard', async ({ browser }) => {
    // The T-5.5.3 flow with the expiry defect sidestepped: proves the propose
    // picker, the dashboard card and the swap itself all work.
    const m = ids();
    const offered = seedTradeChore(m.owner, 'Trade offered');
    const requested = seedTradeChore(m.adult, 'Trade requested');

    const owner = await session(browser, PERSONAS.owner);
    const flow = await proposeThenAcceptFromDashboard(browser, owner, offered, requested);
    expect(flow.acceptStatus).toBe(200);
    await expect(flow.card).toBeHidden();

    expect(tradeFor(offered.instanceId).status).toBe('accepted');
    expect(instanceRow(offered.instanceId).assignee, 'the offered chore moves to the recipient').toBe(m.adult);
    expect(instanceRow(requested.instanceId).assignee, 'the requested chore moves to the proposer').toBe(m.owner);

    // The adult's task list now offers the traded-in chore as their own.
    await flow.adult.page.goto('/tasks');
    await expect(flow.adult.page.locator(`#task-${offered.instanceId}`).getByRole('link', { name: 'Trade' })).toBeVisible();

    await owner.context.close();
    await flow.adult.context.close();
  });

  test('T-5.5.6 only the recipient can accept or decline a trade', async ({ browser }) => {
    const m = ids();
    const offered = seedTradeChore(m.owner, 'Trade not yours');
    const requested = seedTradeChore(m.adult, 'Trade not yours req');
    const owner = await session(browser, PERSONAS.owner);
    expect(await propose(owner, offered.instanceId, requested.instanceId)).toBe(303);
    const { id } = tradeFor(offered.instanceId);

    // A bystander (the child) and the proposer themselves are both refused.
    const child = await session(browser, PERSONAS.child);
    expect(await postForm(child.page, `/trades/${id}/accept`, { csrf_token: child.csrf_token })).toBe(409);
    expect(await postForm(child.page, `/trades/${id}/decline`, { csrf_token: child.csrf_token })).toBe(409);
    expect(await postForm(owner.page, `/trades/${id}/accept`, { csrf_token: owner.csrf_token })).toBe(409);

    expect(tradeFor(offered.instanceId).status).toBe('proposed');
    expect(instanceRow(offered.instanceId).assignee).toBe(m.owner);
    expect(instanceRow(requested.instanceId).assignee).toBe(m.adult);

    // Sanity guard: the actual recipient's accept on the same trade succeeds.
    const adult = await session(browser, PERSONAS.adult);
    expect(await postForm(adult.page, `/trades/${id}/accept`, { csrf_token: adult.csrf_token })).toBe(303);
    expect(instanceRow(offered.instanceId).assignee).toBe(m.adult);

    for (const s of [owner, child, adult]) await s.context.close();
  });

  test('T-5.5.7 a completed chore cannot be traded, and completing one mid-trade blocks the accept', async ({ browser }) => {
    const m = ids();
    const owner = await session(browser, PERSONAS.owner);
    const adult = await session(browser, PERSONAS.adult);

    // Offering a chore that is already done.
    const done = seedTradeChore(m.owner, 'Trade done');
    const target = seedTradeChore(m.adult, 'Trade target');
    expect(await postForm(owner.page, `/tasks/${done.instanceId}/complete`, { csrf_token: owner.csrf_token })).toBe(303);
    expect(await propose(owner, done.instanceId, target.instanceId), 'ErrInstanceNotTradeable').toBe(409);
    const picker = await owner.page.request.get(`/tasks/${done.instanceId}/propose-trade`);
    expect(picker.status(), 'the picker refuses a finished chore too').toBe(409);
    expect(tradeFor(done.instanceId)).toBeNull();

    // A live trade whose requested chore gets completed before the accept.
    const offered = seedTradeChore(m.owner, 'Trade midway');
    const requested = seedTradeChore(m.adult, 'Trade midway req');
    expect(await propose(owner, offered.instanceId, requested.instanceId), 'sanity: a pending pair trades').toBe(303);
    const { id } = tradeFor(offered.instanceId);
    expect(await postForm(adult.page, `/tasks/${requested.instanceId}/complete`, { csrf_token: adult.csrf_token })).toBe(303);
    expect(await postForm(adult.page, `/trades/${id}/accept`, { csrf_token: adult.csrf_token })).toBe(409);
    expect(instanceRow(offered.instanceId).assignee, 'a refused accept swaps nothing').toBe(m.owner);
    expect(tradeFor(offered.instanceId).status).toBe('proposed');

    await owner.context.close();
    await adult.context.close();
  });

  test('T-5.5.8 a claimed chore is not tradeable: it cannot be offered, and claiming mid-trade blocks the accept', async ({ browser }) => {
    const m = ids();
    const owner = await session(browser, PERSONAS.owner);
    const adult = await session(browser, PERSONAS.adult);

    // An open claimable chore the owner claims, then tries to offer.
    const open = seedTradeChore(null, 'Trade claimed', { policy: 'claimable' });
    const target = seedTradeChore(m.adult, 'Trade claimed target');
    expect(await postForm(owner.page, `/tasks/${open.instanceId}/claim`, { csrf_token: owner.csrf_token })).toBe(303);
    expect(instanceRow(open.instanceId).assignee).toBe(m.owner);
    expect(await propose(owner, open.instanceId, target.instanceId)).toBe(409);
    expect((await owner.page.request.get(`/tasks/${open.instanceId}/propose-trade`)).status()).toBe(409);

    // A live trade whose requested chore its assignee then claims.
    const offered = seedTradeChore(m.owner, 'Trade claim midway');
    const requested = seedTradeChore(m.adult, 'Trade claim midway req');
    expect(await propose(owner, offered.instanceId, requested.instanceId), 'sanity: an unclaimed pair trades').toBe(303);
    const { id } = tradeFor(offered.instanceId);
    expect(await postForm(adult.page, `/tasks/${requested.instanceId}/claim`, { csrf_token: adult.csrf_token })).toBe(303);
    expect(await postForm(adult.page, `/trades/${id}/accept`, { csrf_token: adult.csrf_token })).toBe(409);
    expect(instanceRow(offered.instanceId).assignee).toBe(m.owner);
    expect(instanceRow(requested.instanceId).assignee).toBe(m.adult);

    await owner.context.close();
    await adult.context.close();
  });

  test('T-5.5.9 trade history is open to parents and refused to a child', async ({ browser }) => {
    const m = ids();
    const offered = seedTradeChore(m.owner, 'Trade history');
    const requested = seedTradeChore(m.adult, 'Trade history req');
    const owner = await session(browser, PERSONAS.owner);
    expect(await propose(owner, offered.instanceId, requested.instanceId)).toBe(303);
    const { id } = tradeFor(offered.instanceId);
    expect(await postForm(owner.page, `/trades/${id}/cancel`, { csrf_token: owner.csrf_token })).toBe(303);

    for (const persona of [PERSONAS.owner, PERSONAS.adult]) {
      const s = persona === PERSONAS.owner ? owner : await session(browser, persona);
      const res = await s.page.goto('/trades/history');
      expect(res.status(), `${persona.role} sees history`).toBe(200);
      await expect(s.page.locator('#main-content')).toContainText(offered.title);
      if (s !== owner) await s.context.close();
    }

    const child = await session(browser, PERSONAS.child);
    const refused = await child.page.goto('/trades/history');
    expect(refused.status()).toBe(403);
    await expect(child.page.locator('body')).not.toContainText(offered.title);

    await owner.context.close();
    await child.context.close();
  });

  test('T-5.5.10 one chore can be in only one live trade, even when two members propose at once', async ({ browser }) => {
    const m = ids();
    const owner = await session(browser, PERSONAS.owner);
    const child = await session(browser, PERSONAS.child);
    const adult = await session(browser, PERSONAS.adult);

    // Sequential: once the owner's chore is in a trade, nobody can put it in a
    // second one — neither the owner offering it again nor the child asking
    // for it.
    const shared = seedTradeChore(m.owner, 'Trade shared');
    const adultChore = seedTradeChore(m.adult, 'Trade shared adult');
    const childChore = seedTradeChore(m.child, 'Trade shared child');
    expect(await propose(owner, shared.instanceId, adultChore.instanceId)).toBe(303);
    expect(await propose(owner, shared.instanceId, childChore.instanceId)).toBe(409);
    expect(await propose(child, childChore.instanceId, shared.instanceId)).toBe(409);
    expect(liveTradesTouching(shared.instanceId)).toBe(1);

    // Concurrent: the owner offers X while the child asks for X, at once.
    const x = seedTradeChore(m.owner, 'Trade race');
    const y = seedTradeChore(m.adult, 'Trade race adult');
    const c = seedTradeChore(m.child, 'Trade race child');
    const statuses = await Promise.all([
      propose(owner, x.instanceId, y.instanceId),
      propose(child, c.instanceId, x.instanceId),
    ]);
    expect(statuses.filter((s) => s === 303), `exactly one proposal may win: ${statuses}`).toHaveLength(1);
    expect(statuses.filter((s) => s !== 303)).toEqual([409]);
    expect(liveTradesTouching(x.instanceId)).toBe(1);

    // The single surviving trade can still be accepted by its recipient.
    const winner = psql(
      `SELECT id || '|' || responder_id FROM nestova.chore_trade
        WHERE status = 'proposed' AND (offered_instance_id = '${x.instanceId}' OR requested_instance_id = '${x.instanceId}');`,
    ).trim().split('|');
    const recipient = winner[1] === m.adult ? adult : owner;
    expect(await postForm(recipient.page, `/trades/${winner[0]}/accept`, { csrf_token: recipient.csrf_token })).toBe(303);

    for (const s of [owner, child, adult]) await s.context.close();
  });

  test('T-5.5.11 a recipient removed from the household before accepting takes the trade with them', async ({ browser }) => {
    // Nestova has no remove-member feature, so removal is simulated the only
    // way it can happen today: deleting the member row. What is pinned is the
    // schema's defined outcome — the trade cascades away, the proposer's chore
    // is released for a new trade, and the leaver's chore falls unassigned.
    const m = ids();
    const suffix = Date.now();
    const leaver = seedMemberInA({
      displayName: `Leaver ${suffix}`, email: `leaver-${suffix}@test.local`, copyHashFrom: PERSONAS.owner.email,
    });
    const offered = seedTradeChore(m.owner, 'Trade leaver');
    const leaversChore = seedTradeChore(leaver, 'Trade leaver req');
    const owner = await session(browser, PERSONAS.owner);
    expect(await propose(owner, offered.instanceId, leaversChore.instanceId)).toBe(303);
    const { id } = tradeFor(offered.instanceId);
    await owner.page.goto('/');
    await expect(owner.page.locator(`#trade-${id}`)).toBeVisible();

    psql(`DELETE FROM identity.member WHERE id = '${leaver}';`);

    expect(tradeFor(offered.instanceId), 'the trade is gone with its recipient').toBeNull();
    expect(instanceRow(leaversChore.instanceId).assignee, "the leaver's chore is unassigned").toBe('');
    await owner.page.goto('/');
    await expect(owner.page.locator(`#trade-${id}`)).toHaveCount(0);

    // Accepting the vanished trade, or trading for the orphaned chore, is
    // refused cleanly.
    const adult = await session(browser, PERSONAS.adult);
    expect(await postForm(adult.page, `/trades/${id}/accept`, { csrf_token: adult.csrf_token })).toBe(409);
    expect(await propose(owner, offered.instanceId, leaversChore.instanceId)).toBe(409);

    // Sanity guard: the proposer's chore is free to trade again.
    const another = seedTradeChore(m.adult, 'Trade leaver retry');
    expect(await propose(owner, offered.instanceId, another.instanceId)).toBe(303);

    await owner.context.close();
    await adult.context.close();
  });

  test('T-5.5.12 after a trade, completing the chore credits the new assignee, not the old one', async ({ browser }) => {
    // Seeded members with PINs, so the NES-166 gate decides who gets credited:
    // after the trade the chore's PIN is the NEW assignee's.
    const suffix = Date.now();
    const pia = { name: `Pia ${suffix}`, email: `pia-${suffix}@test.local`, pin: '4821' };
    const nils = { name: `Nils ${suffix}`, email: `nils-${suffix}@test.local`, pin: '1357' };
    for (const kid of [pia, nils]) {
      kid.id = seedMemberInA({ displayName: kid.name, email: kid.email, role: 'child', copyHashFrom: PERSONAS.owner.email });
    }
    const piaChore = seedTradeChore(pia.id, 'Trade points pia', { points: 7 });
    const nilsChore = seedTradeChore(nils.id, 'Trade points nils', { points: 3 });

    const owner = await session(browser, PERSONAS.owner);
    for (const kid of [pia, nils]) {
      await owner.page.goto('/settings');
      const row = owner.page.locator('li').filter({ hasText: kid.name }).first();
      await row.locator('input[name="pin"]').fill(kid.pin);
      await row.getByRole('button', { name: /^(Set|Change)$/ }).click();
      await expect(owner.page.locator('li').filter({ hasText: kid.name }).first()).toContainText('PIN set');
    }

    const piaSession = await session(browser, { email: pia.email, password: PERSONAS.owner.password });
    const nilsSession = await session(browser, { email: nils.email, password: PERSONAS.owner.password });
    expect(await propose(piaSession, piaChore.instanceId, nilsChore.instanceId)).toBe(303);
    const { id } = tradeFor(piaChore.instanceId);
    expect(await postForm(nilsSession.page, `/trades/${id}/accept`, { csrf_token: nilsSession.csrf_token })).toBe(303);
    expect(instanceRow(piaChore.instanceId).assignee).toBe(nils.id);

    // Pia's own PIN no longer opens her former chore.
    const page = piaSession.page;
    await page.goto('/tasks');
    const row = () => page.locator(`#task-${piaChore.instanceId}`);
    await row().getByTestId('task-pin-input').fill(pia.pin);
    await row().getByRole('button', { name: 'Done' }).click();
    await expect(row().getByTestId('task-pin-error')).toBeVisible();
    expect(instanceRow(piaChore.instanceId).status).toBe('pending');

    // Nils's PIN completes it, and the 7 points land with Nils even though the
    // screen is signed in as Pia.
    await row().getByTestId('task-pin-input').fill(nils.pin);
    await row().getByRole('button', { name: 'Done' }).click();
    await expect(row().getByText('Completed')).toBeVisible();

    const credited = psql(
      `SELECT member_id || '|' || points FROM nestova.point_ledger WHERE source_id = '${piaChore.instanceId}';`,
    ).trim();
    expect(credited, 'exactly one award, to the new assignee').toBe(`${nils.id}|7`);
    expect(instanceRow(piaChore.instanceId).completedBy).toBe(nils.id);

    for (const s of [owner, piaSession, nilsSession]) await s.context.close();
  });
});
