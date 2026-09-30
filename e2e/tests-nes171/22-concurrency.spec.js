// §0.7 concurrency and double-submit — the cases not already in 10-rewards and
// 11-chores: double-clicked submit buttons, trades resolved twice or from both
// sides at once, a form resubmitted from browser history, and one photo
// uploaded twice at the same moment.
//
// Races fire with Promise.all and no await between the requests; where the
// race only exists between two members (accepting versus cancelling a trade)
// the two requests come from two members' own sessions.
const { test, expect } = require('@playwright/test');
const crypto = require('crypto');
const fs = require('fs');
const { PERSONAS } = require('../tests/fixtures');
const { login, postForm } = require('./helpers');
const { psql } = require('../tests/db');
const fuzz = require('./helpers-fuzz');
const fixtures = require('./media-fixtures');

// How long a double-clicked form is given to finish every request it started
// before its rows are counted.
const SETTLE_MS = 2_000;

function countRows(table, column, marker) {
  return fuzz.storedValues(table, column, marker).length;
}

// CREATE_FORMS drives each create form through its real UI. fill() puts a
// complete, valid entry carrying marker into the form; the row is then found
// by marker in table.column.
const CREATE_FORMS = [
  {
    name: 'task',
    page: '/tasks/new',
    form: 'form[action="/tasks"]',
    table: 'nestova.recurring_task',
    column: 'title',
    fill: async (form, marker) => {
      await form.locator('input[name="title"]').fill(`Double ${marker}`);
      await form.locator('input[name="interval"]').fill('1');
      await form.locator('input[name="pool"]').first().check();
    },
  },
  {
    name: 'reward',
    page: '/admin/rewards/new',
    form: 'form[action="/admin/rewards"]',
    table: 'nestova.reward',
    column: 'name',
    fill: async (form, marker) => {
      await form.locator('input[name="name"]').fill(`Double ${marker}`);
      await form.locator('input[name="cost_points"]').fill('5');
    },
  },
  {
    name: 'member',
    page: '/members/new',
    form: 'form[action="/members"]',
    table: 'identity.member',
    column: 'display_name',
    fill: async (form, marker) => {
      await form.locator('input[name="display_name"]').fill(`Double ${marker}`);
      await form.locator('select[name="role"]').selectOption('child');
    },
    cleanup: (marker) => psql(`DELETE FROM identity.member WHERE display_name LIKE '%${marker}%';`),
  },
  {
    name: 'album',
    page: '/photos',
    form: 'form[action="/albums"]',
    table: 'nestova.album',
    column: 'name',
    fill: async (form, marker) => {
      await form.locator('input[name="name"]').fill(`Double ${marker}`);
    },
  },
  {
    name: 'recipe',
    page: '/meals',
    form: 'form[action="/meals/recipes"]',
    table: 'nestova.recipe',
    column: 'title',
    open: (page) => page.getByText('Add a recipe').click(),
    fill: async (form, marker) => {
      await form.locator('input[name="title"]').fill(`Double ${marker}`);
      await form.locator('input[name="servings"]').fill('2');
      await form.locator('input[name="ingredient_name"]').first().fill('flour');
      await form.locator('input[name="ingredient_amount"]').first().fill('100');
      await form.locator('select[name="ingredient_unit"]').first().selectOption('g');
    },
  },
  {
    name: 'subscription',
    page: '/subscriptions',
    form: 'form[action="/subscriptions"]',
    table: 'nestova.subscription',
    column: 'name',
    fill: async (form, marker) => {
      await form.locator('input[name="name"]').fill(`Double ${marker}`);
      await form.locator('input[name="amount"]').fill('4.99');
      await form.locator('input[name="next_renewal_on"]').fill(fuzz.isoDate(30));
    },
  },
  {
    name: 'shopping item',
    page: '/groceries',
    form: 'form[action="/groceries/shopping"]',
    table: 'nestova.shopping_list_item',
    column: 'name',
    fill: async (form, marker) => {
      await form.locator('input[name="name"]').fill(`Double ${marker}`);
      await form.locator('input[name="amount"]').fill('1');
    },
  },
];

// ---------------------------------------------------------------------------
// Trades
// ---------------------------------------------------------------------------

// seedInstance gives assignee a pending chore due in two days. A trade expires
// at the start (UTC) of the earlier chore's due date, so a chore due today
// would make a trade that is already expired when it is proposed.
function seedInstance(assignee) {
  const household = fuzz.householdA();
  const taskId = psql(`
    INSERT INTO nestova.recurring_task
      (id, household_id, title, category, cadence, rotation_policy, points, lead_time_days, active, created_at, updated_at)
    VALUES (gen_random_uuid(), '${household}', 'Trade race probe', 'chore',
            '{"Freq":"daily","Interval":1,"Anchor":"2026-08-25T00:00:00Z","ByWeekday":null}'::jsonb,
            'fixed', 5, 0, true, now(), now())
    RETURNING id;`).trim();
  return psql(`
    INSERT INTO nestova.task_instance
      (id, recurring_task_id, household_id, assignee_id, due_on, status, kind, created_at, updated_at)
    VALUES (gen_random_uuid(), '${taskId}', '${household}', '${assignee}', current_date + 2, 'pending', 'scheduled', now(), now())
    RETURNING id;`).trim();
}

function assigneeOf(instanceId) {
  return psql(`SELECT assignee_id FROM nestova.task_instance WHERE id = '${instanceId}';`).trim();
}

function tradeStatus(tradeId) {
  return psql(`SELECT status FROM nestova.chore_trade WHERE id = '${tradeId}';`).trim();
}

// memberSession signs persona in on a context of its own, so two members can
// act at the same moment.
async function memberSession(browser, persona) {
  const context = await browser.newContext();
  const page = await context.newPage();
  await login(page, persona);
  const csrf_token = await fuzz.csrfFor(page, '/tasks');
  return { context, page, csrf_token };
}

// proposeTrade has the owner offer one of their chores for one of the child's,
// and returns the trade and both instance ids.
async function proposeTrade(owner) {
  const ownerId = fuzz.memberId(PERSONAS.owner.displayName);
  const childId = fuzz.memberId(PERSONAS.child.displayName);
  const offered = seedInstance(ownerId);
  const requested = seedInstance(childId);
  const status = await postForm(owner.page, '/trades', {
    csrf_token: owner.csrf_token, offered_instance_id: offered, requested_instance_id: requested,
  });
  expect(status, 'a valid trade proposal must be accepted').toBe(303);
  const tradeId = psql(`SELECT id FROM nestova.chore_trade WHERE offered_instance_id = '${offered}' LIMIT 1;`).trim();
  expect(tradeId, 'the proposed trade must exist').toBeTruthy();
  return { tradeId, offered, requested, ownerId, childId };
}

test.describe('§0.7 concurrency and double-submit', () => {
  // Kept apart from T-0.7.1 because test.fail accepts any failure: a broken fill
  // inside that test would be reported as the expected defect.
  test('sanity: every create form makes one record on a single click', async ({ page }) => {
    test.setTimeout(180_000);
    await login(page, PERSONAS.owner);

    for (const spec of CREATE_FORMS) {
      const marker = fuzz.uniqueMarker('one');
      try {
        await page.goto(spec.page);
        if (spec.open) await spec.open(page);
        const form = page.locator(spec.form);
        await spec.fill(form, marker);
        await form.locator('button[type="submit"]').click();
        await expect.poll(() => countRows(spec.table, spec.column, marker), { message: `${spec.name} records` }).toBe(1);
      } finally {
        if (spec.cleanup) spec.cleanup(marker);
      }
    }
  });

  test('T-0.7.1 [!] double-clicking a create form\'s submit button makes one record', async ({ page }) => {
    test.setTimeout(180_000);
    await login(page, PERSONAS.owner);

    const failures = [];
    for (const spec of CREATE_FORMS) {
      const marker = fuzz.uniqueMarker('dbl');
      await page.goto(spec.page);
      if (spec.open) await spec.open(page);
      const form = page.locator(spec.form);
      await spec.fill(form, marker);
      await form.locator('button[type="submit"]').dblclick();
      await page.waitForTimeout(SETTLE_MS);
      const rows = countRows(spec.table, spec.column, marker);
      if (spec.cleanup) spec.cleanup(marker);
      if (rows !== 1) failures.push(`${spec.name}: ${rows} records`);
    }
    expect(failures, 'forms that created more than one record from a double click').toEqual([]);
  });

  test('T-0.7.6 accepting the same trade twice: the second gets 409', async ({ browser }) => {
    const owner = await memberSession(browser, PERSONAS.owner);
    const child = await memberSession(browser, PERSONAS.child);
    const accept = (tradeId) => postForm(child.page, `/trades/${tradeId}/accept`, { csrf_token: child.csrf_token });
    // Swapped exactly once: a second swap would put both chores back.
    const expectSwappedOnce = ({ tradeId, offered, requested, ownerId, childId }) => {
      expect(tradeStatus(tradeId)).toBe('accepted');
      expect(assigneeOf(offered), 'the offered chore moves to the child').toBe(childId);
      expect(assigneeOf(requested), 'the requested chore moves to the owner').toBe(ownerId);
    };
    try {
      const sequential = await proposeTrade(owner);
      expect(await accept(sequential.tradeId), 'the first accept must succeed').toBe(303);
      expect(await accept(sequential.tradeId), 'ErrTradeNotPending must map to 409').toBe(409);
      expectSwappedOnce(sequential);

      // The same, as a double-clicked Accept: both requests in flight at once.
      const doubled = await proposeTrade(owner);
      const statuses = await Promise.all([accept(doubled.tradeId), accept(doubled.tradeId)]);
      expect(statuses.sort(), 'one accept wins and the other gets 409').toEqual([303, 409]);
      expectSwappedOnce(doubled);
    } finally {
      await owner.context.close();
      await child.context.close();
    }
  });

  test('T-0.7.7 accept and cancel racing on one trade reach one consistent terminal state', async ({ browser }) => {
    const owner = await memberSession(browser, PERSONAS.owner);
    const child = await memberSession(browser, PERSONAS.child);
    try {
      const { tradeId, offered, requested, ownerId, childId } = await proposeTrade(owner);

      // The responder accepts while the proposer withdraws, at the same moment.
      const [accept, cancel] = await Promise.all([
        postForm(child.page, `/trades/${tradeId}/accept`, { csrf_token: child.csrf_token }),
        postForm(owner.page, `/trades/${tradeId}/cancel`, { csrf_token: owner.csrf_token }),
      ]);

      const statuses = [accept, cancel];
      test.info().annotations.push({ type: 'race', description: `accept -> ${accept}, cancel -> ${cancel}` });
      expect(statuses.filter((s) => s === 303), 'exactly one of accept and cancel may win').toHaveLength(1);
      expect(statuses.filter((s) => s === 409), 'the loser must get 409, not a 5xx').toHaveLength(1);

      const final = tradeStatus(tradeId);
      if (accept === 303) {
        expect(final).toBe('accepted');
        expect(assigneeOf(offered)).toBe(childId);
        expect(assigneeOf(requested)).toBe(ownerId);
      } else {
        expect(final).toBe('cancelled');
        expect(assigneeOf(offered), 'a cancelled trade must not move the chores').toBe(ownerId);
        expect(assigneeOf(requested)).toBe(childId);
      }
    } finally {
      await owner.context.close();
      await child.context.close();
    }
  });

  test('T-0.7.8 [!] going back and resubmitting a completed form makes no duplicate', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const reward = CREATE_FORMS.find((f) => f.name === 'reward');
    const marker = fuzz.uniqueMarker('back');

    await page.goto(reward.page);
    await reward.fill(page.locator(reward.form), marker);
    await page.locator(reward.form).locator('button[type="submit"]').click();
    await page.waitForURL((u) => new URL(u).pathname === '/admin/rewards');
    expect(countRows(reward.table, reward.column, marker), 'the first submission creates the reward').toBe(1);

    // Post/Redirect/Get: reloading the result page and going back and forward
    // through history must not replay the POST.
    await page.reload();
    await page.goBack();
    await page.goForward();
    await page.goBack();
    expect(countRows(reward.table, reward.column, marker), 'history navigation must not replay the POST').toBe(1);

    // Back on the form: the user presses submit again on the completed entry.
    // If the browser did not restore the values, re-enter the same ones.
    const form = page.locator(reward.form);
    if ((await form.locator('input[name="name"]').inputValue()) === '') await reward.fill(form, marker);
    const [resubmit] = await Promise.all([
      page.waitForResponse((r) => r.request().method() === 'POST' && new URL(r.url()).pathname === '/admin/rewards'),
      form.locator('button[type="submit"]').click(),
    ]);
    expect(resubmit.status(), 'a spent form token must be rejected with 409').toBe(409);
    await expect(page.getByRole('alert')).toContainText('already submitted');
    expect(countRows(reward.table, reward.column, marker), 'a resubmitted form must not create a duplicate').toBe(1);
  });

  test('T-0.7.9 one photo uploaded twice at once is stored once, without a 5xx', async ({ page }) => {
    test.skip(!fixtures.hasMagick(), 'ImageMagick is required to generate the image fixture');
    await login(page, PERSONAS.owner);
    const bytes = fs.readFileSync(fixtures.noise(`race-${Date.now()}.png`));
    const sha256 = crypto.createHash('sha256').update(bytes).digest('hex');
    const csrf_token = await fuzz.csrfFor(page, '/photos');
    const upload = (caption) => page.request.post('/photos', {
      maxRedirects: 0,
      multipart: { csrf_token, caption, photo: { name: 'race.png', mimeType: 'image/png', buffer: bytes } },
    });

    try {
      // NES-148 dedups on the content hash; two uploads of the same bytes at
      // the same moment race to the unique (household, sha256) index.
      const [a, b] = await Promise.all([upload('Race a'), upload('Race b')]);
      const results = [a, b].map((r) => ({ status: r.status(), result: r.headers()['x-upload-result'] || null }));

      expect(results.map((r) => r.status), 'neither racing upload may fail').toEqual([303, 303]);
      expect(results.map((r) => r.result).sort(), 'one upload creates, the other is reported as a duplicate')
        .toEqual(['created', 'duplicate']);
      const stored = psql(`SELECT count(*) FROM nestova.photo WHERE content_sha256 = '${sha256}';`).trim();
      expect(stored, 'the content must be stored once').toBe('1');
    } finally {
      fixtures.cleanup();
    }
  });
});
