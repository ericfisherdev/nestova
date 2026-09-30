// §0.8 HTMX and rendering integrity.
//
// The in-place contract is narrower than "every button": chore rows
// (claim/complete/skip), trade cards (accept/decline/cancel) and the
// redemption inbox (fulfil/deny) swap their own element. Groceries, meals,
// photos and subscriptions answer an HTMX mutation with HX-Redirect on
// purpose — "so the whole page refreshes and reflects the new state"
// (respondAfterMutation in those adapters) — so those are full reloads by
// design and T-0.8.1 is asserted against the swap routes only.
//
// Layout's htmx-config swaps 2xx/3xx and 422 and nothing else, so the error
// cases split in two: a 422 must be a fragment (it IS swapped), and any other
// 4xx must leave the DOM exactly as it was (it is NOT swapped).
//
// Every test acts as a member seeded for this run with no PIN, so the shared
// personas' PIN state (set by other specs) cannot change which controls
// render.
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, csrfToken, postForm } = require('./helpers');
const { psql, seedMemberInA } = require('../tests/db');
const { householdId, seedInstance, ZERO_UUID } = require('./helpers-isolation');

const TS = Date.now();
let seq = 0;
const unique = (label) => `${label} ${TS}-${++seq}`;

// A LAYOUT marker: any of these in a response body means a full page, not a
// fragment.
const FULL_PAGE = /<html|<head|<body/i;

let household;
let actor;

test.beforeAll(() => {
  household = householdId('Household A');
  const email = `htmx-${TS}@test.local`;
  const id = seedMemberInA({ displayName: `Htmx ${TS}`, email, role: 'adult', copyHashFrom: PERSONAS.owner.email });
  actor = { id, email, password: PERSONAS.owner.password };
});

// watchNavigations counts main-frame navigations from here on and plants a
// marker on window. A fragment swap keeps both; a full navigation loses the
// marker and bumps the count.
async function watchNavigations(page) {
  const counter = { navigations: 0 };
  page.on('framenavigated', (frame) => {
    if (frame === page.mainFrame()) counter.navigations++;
  });
  await page.evaluate(() => { window.__noNavigation = true; });
  counter.stillSamePage = () => page.evaluate(() => window.__noNavigation === true);
  return counter;
}

// htmxPost posts as htmx would (HX-Request: true) and returns what htmx would
// see: status, body, and any HX-Redirect.
async function htmxPost(page, path, fields) {
  return page.evaluate(async ({ path, fields }) => {
    const res = await fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'HX-Request': 'true' },
      body: new URLSearchParams(fields).toString(),
      redirect: 'manual',
    });
    return { status: res.status, body: await res.text(), hxRedirect: res.headers.get('HX-Redirect') };
  }, { path, fields });
}

function instanceStatus(id) {
  return psql(`SELECT status FROM nestova.task_instance WHERE id = '${id}';`).trim();
}

function seedRedemption() {
  const child = psql(`SELECT id FROM identity.member WHERE email = '${PERSONAS.child.email}';`).trim();
  const reward = psql(`
    INSERT INTO nestova.reward (id, household_id, name, cost_points, active, quantity_available, created_at, updated_at)
    VALUES (gen_random_uuid(), '${household}', '${unique('Htmx reward')}', 5, true, 10, now(), now()) RETURNING id;`).trim();
  return psql(`
    INSERT INTO nestova.reward_redemption (id, household_id, reward_id, member_id, status, created_at, updated_at)
    VALUES (gen_random_uuid(), '${household}', '${reward}', '${child}', 'pending', now(), now()) RETURNING id;`).trim();
}

test.describe('§0.8 HTMX and rendering integrity', () => {
  test('T-0.8.1 in-place actions swap a fragment without navigating', async ({ page }) => {
    const claimable = seedInstance(household, { title: unique('Htmx claim') });
    const toComplete = seedInstance(household, { assignee: actor.id, title: unique('Htmx complete') });
    const toSkip = seedInstance(household, { assignee: actor.id, title: unique('Htmx skip') });
    const redemption = seedRedemption();
    const child = psql(`SELECT id FROM identity.member WHERE email = '${PERSONAS.child.email}';`).trim();
    const offered = seedInstance(household, { assignee: actor.id, title: unique('Htmx offered') });
    const requested = seedInstance(household, { assignee: child, title: unique('Htmx requested') });

    await login(page, actor);
    const token = await csrfToken(page, '/tasks');
    expect(await postForm(page, '/trades', { csrf_token: token, offered_instance_id: offered, requested_instance_id: requested })).toBe(303);
    const trade = psql(`SELECT id FROM nestova.chore_trade WHERE offered_instance_id = '${offered}' LIMIT 1;`).trim();

    await page.goto('/tasks');
    const tasks = await watchNavigations(page);
    await page.locator(`#task-${claimable}`).getByRole('button', { name: 'Claim' }).click();
    await expect(page.locator(`#task-${claimable}`).getByRole('button', { name: 'Done' }), 'a claimed row offers Done in place').toBeVisible();
    await page.locator(`#task-${toComplete}`).getByRole('button', { name: 'Done' }).click();
    await expect(page.locator(`#task-${toComplete}`)).toContainText('Completed');
    await page.locator(`#task-${toSkip}`).getByRole('button', { name: 'Skip' }).click();
    await expect(page.locator(`#task-${toSkip}`)).toContainText('Skipped');
    expect(instanceStatus(toComplete)).toBe('done');
    expect(tasks.navigations, 'chore actions must not navigate').toBe(0);
    expect(await tasks.stillSamePage(), 'chore actions must keep the same document').toBe(true);

    await page.goto('/admin/rewards');
    const inbox = await watchNavigations(page);
    await page.locator(`#redemption-${redemption}`).getByRole('button', { name: 'Fulfill' }).click();
    await expect(page.locator(`#redemption-${redemption}`)).toBeHidden();
    expect(psql(`SELECT status FROM nestova.reward_redemption WHERE id = '${redemption}';`).trim()).not.toBe('pending');
    expect(inbox.navigations, 'fulfilling must not navigate').toBe(0);
    expect(await inbox.stillSamePage()).toBe(true);

    await page.goto('/');
    const dashboard = await watchNavigations(page);
    await page.locator(`#trade-${trade}`).getByRole('button', { name: 'Cancel' }).click();
    await expect(page.locator(`#trade-${trade}`)).toBeHidden();
    expect(psql(`SELECT status FROM nestova.chore_trade WHERE id = '${trade}';`).trim()).not.toBe('proposed');
    expect(dashboard.navigations, 'cancelling a trade must not navigate').toBe(0);
    expect(await dashboard.stillSamePage()).toBe(true);
  });

  test('T-0.8.2 a failed HTMX POST leaves the row in its previous state', async ({ page, browser }) => {
    // (a) A plain 4xx is not swapped: the chore is finished behind the page's
    // back, so Done answers 409 and the row must stay exactly as rendered.
    const title = unique('Htmx stale');
    const stale = seedInstance(household, { assignee: actor.id, title });
    // (b) A 422 IS swapped: a PIN refusal re-renders the row with a message,
    // and the row must come back whole and still actionable.
    const pinnedEmail = `htmx-pinned-${TS}@test.local`;
    const pinned = seedMemberInA({ displayName: `Pinned ${TS}`, email: pinnedEmail, role: 'child', copyHashFrom: PERSONAS.owner.email });
    const pinContext = await browser.newContext();
    const pinPage = await pinContext.newPage();
    await login(pinPage, { email: pinnedEmail, password: PERSONAS.owner.password });
    const pinToken = await csrfToken(pinPage, '/settings');
    expect(await postForm(pinPage, '/settings/pin', { csrf_token: pinToken, pin: '4821' })).toBe(200);
    await pinContext.close();
    const gatedTitle = unique('Htmx gated');
    const gated = seedInstance(household, { assignee: pinned, title: gatedTitle });

    await login(page, actor);
    await page.goto('/tasks');
    const staleRow = page.locator(`#task-${stale}`);
    const before = await staleRow.innerHTML();

    psql(`UPDATE nestova.task_instance SET status = 'skipped', updated_at = now() WHERE id = '${stale}';`);
    const response = page.waitForResponse((r) => r.url().endsWith(`/tasks/${stale}/complete`));
    await staleRow.getByRole('button', { name: 'Done' }).click();
    expect((await response).status(), 'completing a finished chore is refused').toBeGreaterThanOrEqual(400);
    await page.waitForTimeout(300); // let htmx finish its (non-)swap
    expect(await staleRow.innerHTML(), 'a refused POST must leave the row untouched').toBe(before);
    await expect(staleRow).toContainText(title);

    const gatedRow = page.locator(`#task-${gated}`);
    await gatedRow.getByTestId('task-pin-input').fill('0000');
    const refused = page.waitForResponse((r) => r.url().endsWith(`/tasks/${gated}/complete`));
    await gatedRow.getByRole('button', { name: 'Done' }).click();
    expect((await refused).status(), 'a wrong PIN is refused with 422').toBe(422);
    await expect(gatedRow, 'the refused row keeps its title').toContainText(gatedTitle);
    await expect(gatedRow.getByRole('button', { name: 'Done' }), 'the refused row stays actionable').toBeVisible();
    await expect(gatedRow.getByTestId('task-pin-input')).toBeVisible();
    await expect(page.locator(`#task-${gated}`), 'the swap must not duplicate the row').toHaveCount(1);
    expect(instanceStatus(gated), 'a refused PIN must not complete the chore').toBe('pending');
  });

  test('T-0.8.3 HTMX error responses are fragments, never a full page', async ({ page, context }) => {
    const terminal = seedInstance(household, { assignee: actor.id, title: unique('Htmx terminal') });
    const good = seedInstance(household, { assignee: actor.id, title: unique('Htmx fragment') });
    await login(page, actor);
    const csrf_token = await csrfToken(page, '/tasks');

    // Sanity guard: a successful HTMX action answers with the row fragment.
    const ok = await htmxPost(page, `/tasks/${good}/complete`, { csrf_token });
    expect(ok.status).toBe(200);
    expect(ok.body).toContain(`id="task-${good}"`);
    expect(ok.body, 'a success fragment must not carry the layout').not.toMatch(FULL_PAGE);

    expect((await htmxPost(page, `/tasks/${terminal}/complete`, { csrf_token })).status).toBe(200);
    const cases = {
      'complete a finished chore': [`/tasks/${terminal}/complete`, { csrf_token }],
      'complete an unknown chore': [`/tasks/${ZERO_UUID}/complete`, { csrf_token }],
      'cancel an unknown trade': [`/trades/${ZERO_UUID}/cancel`, { csrf_token }],
      'fulfil an unknown redemption': [`/admin/rewards/redemptions/${ZERO_UUID}/fulfill`, { csrf_token }],
      'consume an unknown pantry item': [`/groceries/pantry/${ZERO_UUID}/consume`, { csrf_token, amount: '1', unit: 'g' }],
      'delete an unknown recipe': [`/meals/recipes/${ZERO_UUID}/delete`, { csrf_token }],
      'deactivate an unknown subscription': [`/subscriptions/${ZERO_UUID}/deactivate`, { csrf_token }],
      'delete an unknown photo': [`/photos/${ZERO_UUID}/delete`, { csrf_token }],
      'post with a forged CSRF token': [`/tasks/${good}/skip`, { csrf_token: 'forged' }],
    };
    const fullPages = [];
    for (const [label, [path, fields]] of Object.entries(cases)) {
      const res = await htmxPost(page, path, fields);
      expect(res.status, `${label}: must be a 4xx`).toBeGreaterThanOrEqual(400);
      expect(res.status, `${label}: must not be a server error`).toBeLessThan(500);
      if (FULL_PAGE.test(res.body)) fullPages.push(`${label} -> ${res.status}`);
    }
    expect(fullPages, 'HTMX errors that answered with a full page').toEqual([]);

    // A session that ended mid-page: htmx must get a 401, not the login page
    // injected into a row.
    await context.clearCookies();
    const signedOut = await htmxPost(page, `/tasks/${good}/skip`, { csrf_token });
    expect(signedOut.status, 'a signed-out HTMX request gets 401').toBe(401);
    expect(signedOut.body).not.toMatch(FULL_PAGE);
  });

  test('T-0.8.4 interactive elements show a pointer cursor', async ({ page }) => {
    await login(page, actor);
    const pages = ['/', '/tasks', '/rewards', '/admin/rewards', '/trades/history', '/groceries', '/meals',
      '/calendar', '/subscriptions', '/photos', '/settings'];
    const offenders = [];
    for (const path of pages) {
      await page.goto(path);
      const { checked, wrong } = await page.evaluate(() => {
        const selector = [
          'button', 'a[href]', 'summary', '[role="button"]', 'input[type="submit"]', 'input[type="button"]',
          '[hx-get]:not(form):not([hx-trigger])', '[hx-post]:not(form):not([hx-trigger])',
        ].join(',');
        const visible = [...document.querySelectorAll(selector)].filter((el) => {
          const r = el.getBoundingClientRect();
          return r.width > 0 && r.height > 0 && getComputedStyle(el).visibility !== 'hidden';
        });
        const wrongOnes = visible
          .filter((el) => getComputedStyle(el).cursor !== (el.disabled ? 'not-allowed' : 'pointer'))
          .map((el) => `<${el.tagName.toLowerCase()}> "${(el.textContent || el.value || '').trim().slice(0, 30)}" cursor=${getComputedStyle(el).cursor}`);
        return { checked: visible.length, wrong: wrongOnes };
      });
      expect(checked, `${path} should have interactive elements to check`).toBeGreaterThan(0);
      offenders.push(...wrong.map((w) => `${path} ${w}`));
    }
    expect(offenders, 'interactive elements without the pointer cursor').toEqual([]);
  });

  test('T-0.8.5 rapid repeated clicks during an in-flight request do not stack swaps', async ({ page }) => {
    const id = seedInstance(household, { assignee: actor.id, title: unique('Htmx rapid') });
    await login(page, actor);
    await page.goto('/tasks');

    // Hold every completion in flight long enough for the extra clicks to land.
    let sent = 0;
    await page.route('**/tasks/*/complete', async (route) => {
      sent++;
      await new Promise((resolve) => setTimeout(resolve, 1500));
      await route.continue();
    });
    const done = page.locator(`#task-${id}`).getByRole('button', { name: 'Done' });
    await done.click();
    await done.click();
    await done.click();

    await expect(page.locator(`#task-${id}`)).toContainText('Completed', { timeout: 10_000 });
    await page.waitForTimeout(2000); // any queued request would have settled by now
    await expect(page.locator(`#task-${id}`), 'exactly one row, not a stack of swaps').toHaveCount(1);
    expect(await page.locator(`#task-${id} [id="task-${id}"]`).count(), 'the row must not be nested inside itself').toBe(0);
    expect(Number(psql(`SELECT count(*) FROM nestova.point_ledger WHERE source_id = '${id}';`).trim()),
      'the chore must be credited once').toBe(1);
    // htmx drops a trigger whose element already has a request in flight, so
    // the extra clicks never reach the server at all.
    expect(sent, 'three clicks during one in-flight request send one request').toBe(1);
  });

  test('T-0.8.6 with JavaScript disabled, core flows still work as plain forms', async ({ browser }) => {
    const claimable = seedInstance(household, { title: unique('NoJS claim') });
    const context = await browser.newContext({ javaScriptEnabled: false });
    const page = await context.newPage();
    await login(page, actor);

    await page.goto('/tasks');
    await page.locator(`#task-${claimable}`).getByRole('button', { name: 'Claim' }).click();
    await page.waitForURL(/\/tasks/);
    await expect(page.locator(`#task-${claimable}`).getByRole('button', { name: 'Done' }), 'claim works without JS').toBeVisible();

    await page.locator(`#task-${claimable}`).getByRole('button', { name: 'Done' }).click();
    await page.waitForURL(/\/tasks/);
    expect(instanceStatus(claimable), 'complete works without JS').toBe('done');

    const item = unique('NoJS soap');
    await page.goto('/groceries');
    await page.locator('#shopping-add-name').fill(item);
    await page.locator('#shopping-add-amount').fill('2');
    await page.getByRole('button', { name: 'Add to list' }).click();
    await page.waitForURL(/\/groceries/);
    await expect(page.getByText(item).first(), 'adding a shopping item works without JS').toBeVisible();
    await context.close();
  });
});
