// §4 Dashboard — zero automated coverage before this run.
const fs = require('fs');
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, postForm, csrfToken } = require('./helpers');
const {
  PRIVATE_BASE_URL,
  privatePsql,
  recreateDatabase,
  migrateDatabase,
  configuredServer,
  removeAllHouseholds,
  onboard,
} = require('./helpers-onboarding');
const fixtures = require('./media-fixtures');

const CARDS = ['Calendar', 'Chores', 'Meals & Recipes', 'Groceries', 'Photos', 'Subscriptions'];
const NAV = ['Calendar', 'Chores', 'Rewards', 'Meals & Recipes', 'Groceries', 'Subscriptions', 'Photos'];

test.describe('§4 dashboard', () => {
  test('T-4.1.1 all six summary cards render', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const missing = [];
    for (const card of CARDS) {
      if (await page.locator('main section h2', { hasText: card }).count() === 0) missing.push(card);
    }
    expect(missing, 'dashboard cards that did not render').toEqual([]);
  });

  test('T-4.1.2 the sidebar offers all seven destinations', async ({ page }) => {
    await login(page, PERSONAS.owner);
    const missing = [];
    for (const label of NAV) {
      if (await page.locator('nav a', { hasText: label }).count() === 0) missing.push(label);
    }
    expect(missing, 'sidebar destinations that were absent').toEqual([]);
  });

  test('T-4.1.3 the active nav pill is marked for assistive tech', async ({ page }) => {
    await login(page, PERSONAS.owner);
    for (const [path, label] of [['/tasks', 'Chores'], ['/photos', 'Photos'], ['/calendar', 'Calendar']]) {
      await page.goto(path);
      const active = page.locator(`nav a[aria-current="page"]`);
      await expect(active, `${path} should mark its own pill current`).toHaveCount(1);
      await expect(active).toContainText(label);
    }
  });

  test('T-4.2.3 a child can load the dashboard', async ({ page }) => {
    await login(page, PERSONAS.child);
    await expect(page.locator('h1', { hasText: 'Dashboard' })).toBeVisible();
  });
});

// §4.2/§4.3 need a BRAND-NEW household (and one filled to an unusual size),
// which household A can never be again. They run against a private server and
// database (helpers-onboarding.js), onboarding a fresh household per test.
test.describe('§4 dashboard on a private household', () => {
  test.describe.configure({ timeout: 120_000 });

  const DB = 'nestova_private_dashboard';
  const CARD_PAGES = ['/calendar', '/tasks', '/meals', '/groceries', '/photos', '/subscriptions'];
  const at = (p) => `${PRIVATE_BASE_URL}${p}`;
  let server;
  let errorsBefore = 0;

  test.beforeAll(async ({}, testInfo) => {
    testInfo.setTimeout(120_000);
    recreateDatabase(DB);
    migrateDatabase(DB);
    server = configuredServer(DB);
    await server.start('/onboarding');
  });

  test.afterAll(async () => {
    await server.stop();
  });

  test.beforeEach(() => {
    removeAllHouseholds(DB);
    errorsBefore = server.serverErrors().length;
  });

  test.afterEach(() => {
    expect(server.serverErrors().slice(errorsBefore), 'the server logged a 5xx or panic').toEqual([]);
  });

  // newHousehold onboards a fresh household and leaves its owner signed in.
  async function newHousehold(page, displayName = `Owner ${Date.now()}`) {
    const stamp = Date.now();
    await onboard(page, {
      householdName: `Dash Household ${stamp}`,
      displayName,
      email: `dash-${stamp}@test.local`,
      password: PERSONAS.owner.password,
    });
    return privatePsql(DB, `SELECT id || ',' || household_id FROM identity.member WHERE role = 'owner' LIMIT 1;`).trim().split(',');
  }

  test('T-4.2.1 a brand-new household sees a helpful empty message on every card', async ({ page }) => {
    await newHousehold(page);
    await page.goto(at('/'));
    for (const card of CARDS) {
      const section = page.locator('main section', { has: page.locator('h2', { hasText: card }) });
      await expect(section, `${card} card`).toBeVisible();
      // A sentence, not a blank card or a bare zero.
      await expect(section.locator('div.text-ink-secondary'), `${card} empty message`).toHaveText(/\w+(\s+\w+){3,}/);
    }
  });

  test('T-4.3.1 with zero data, the dashboard and every card destination render without a 5xx', async ({ page }) => {
    await newHousehold(page);
    const bad = [];
    for (const path of ['/', ...CARD_PAGES]) {
      const res = await page.goto(at(path));
      const heading = await page.locator('main h1').count();
      if (res.status() !== 200 || heading === 0) bad.push(`${path} -> ${res.status()}, h1 x${heading}`);
    }
    expect(bad, 'empty-household pages that failed to render').toEqual([]);
  });

  test('T-4.2.2 a dense household (500 chores, 1,000 photos) still renders promptly', async ({ page }) => {
    const [ownerId, householdId] = await newHousehold(page);

    // One chore and one photo through the real routes, so every stored shape
    // (cadence JSON, storage ref, file on disk) is the app's own...
    const csrf_token = await csrfToken(page, at('/tasks/new'));
    expect(await postForm(page, '/tasks', {
      csrf_token, title: 'Dense chore 0', category: 'chore', freq: 'daily', interval: '1',
      rotation_policy: 'fixed', photo_policy: 'none', points: '5', lead_time_days: '0', pool: ownerId,
    }), 'seed chore').toBe(303);
    const photoToken = await csrfToken(page, at('/photos'));
    const upload = await page.request.post(at('/photos'), {
      maxRedirects: 0,
      multipart: {
        csrf_token: photoToken,
        caption: 'Dense photo 0',
        photo: { name: 'dense.png', mimeType: 'image/png', buffer: fs.readFileSync(fixtures.noise('dashboard-dense.png')) },
      },
    });
    expect(upload.status(), 'seed photo').toBe(303);

    // ...then cloned in SQL to reach the checklist's volumes.
    privatePsql(DB, `
      WITH src AS (SELECT * FROM recurring_task WHERE household_id = '${householdId}' LIMIT 1),
      clones AS (
        INSERT INTO recurring_task (id, household_id, title, category, cadence, rotation_policy, points, lead_time_days, photo_policy)
        SELECT gen_random_uuid(), src.household_id, 'Dense chore ' || n, src.category, src.cadence,
               src.rotation_policy, src.points, src.lead_time_days, src.photo_policy
          FROM src, generate_series(1, 499) AS n
        RETURNING id, household_id
      )
      INSERT INTO rotation_member (household_id, recurring_task_id, member_id, position)
      SELECT household_id, id, '${ownerId}', 0 FROM clones;
      INSERT INTO task_instance (id, household_id, recurring_task_id, assignee_id, due_on, status, kind)
      SELECT gen_random_uuid(), household_id, id, '${ownerId}', current_date, 'pending', 'scheduled'
        FROM recurring_task t
       WHERE household_id = '${householdId}'
         AND NOT EXISTS (SELECT 1 FROM task_instance i WHERE i.recurring_task_id = t.id AND i.due_on = current_date);
      INSERT INTO photo (id, household_id, storage_ref, taken_at, caption, uploaded_by, size_bytes, content_type, storage_backend)
      SELECT gen_random_uuid(), p.household_id, p.storage_ref, p.taken_at, 'Dense photo ' || n, p.uploaded_by,
             p.size_bytes, p.content_type, p.storage_backend
        FROM (SELECT * FROM photo WHERE household_id = '${householdId}' LIMIT 1) p, generate_series(1, 999) AS n;
    `);
    expect(privatePsql(DB, `SELECT (SELECT count(*) FROM recurring_task) || ',' || (SELECT count(*) FROM task_instance) || ',' || (SELECT count(*) FROM photo);`).trim())
      .toBe('500,500,1000');

    const slow = [];
    for (const [path, expectText] of [['/', 'Dashboard'], ['/tasks', 'Dense chore 499'], ['/photos', null]]) {
      const started = Date.now();
      const res = await page.goto(at(path), { waitUntil: 'domcontentloaded', timeout: 30_000 });
      const ms = Date.now() - started;
      if (res.status() !== 200 || ms > 10_000) slow.push(`${path} -> ${res.status()} in ${ms}ms`);
      if (expectText) await expect(page.getByText(expectText).first()).toBeVisible();
    }
    expect(slow, 'pages that failed or took over 10s at volume').toEqual([]);
    // /photos is the last page loaded: every seeded photo is on it, not a truncated slice.
    await expect(page.locator('main figure')).toHaveCount(1000);
  });

  test('T-4.3.2 long member names do not break the sidebar layout', async ({ page }) => {
    await newHousehold(page, `Owner ${'Longname '.repeat(12).trim()}`);
    const csrf_token = await csrfToken(page, at('/members/new'));
    const names = [
      `Member ${'Wordy '.repeat(20).trim()}`,
      `Unbroken${'x'.repeat(80)}`,
    ];
    for (const display_name of names) {
      expect(await postForm(page, '/members', { csrf_token, display_name, role: 'adult' }), display_name).toBe(303);
    }

    await page.goto(at('/'));
    await expect(page.locator('main h1', { hasText: 'Dashboard' })).toBeVisible();
    const layout = await page.evaluate(() => {
      const sidebar = document.getElementById('sidebar');
      const rail = sidebar.getBoundingClientRect();
      const main = document.getElementById('main-content').getBoundingClientRect();
      const names = [...sidebar.querySelectorAll('span.text-sm')];
      const avatars = [...sidebar.querySelectorAll('span[aria-label].rounded-full')];
      return {
        railWidth: Math.round(rail.width),
        mainLeft: Math.round(main.left),
        pageScrollsSideways: document.documentElement.scrollWidth > document.documentElement.clientWidth,
        sidebarScrollsSideways: sidebar.scrollWidth > sidebar.clientWidth,
        namesPastTheRail: names.filter((el) => el.getBoundingClientRect().right > rail.right + 0.5)
          .map((el) => el.textContent.slice(0, 20)),
        squeezedAvatars: avatars.filter((el) => el.getBoundingClientRect().width < 30)
          .map((el) => `${el.getAttribute('aria-label').slice(0, 20)}: ${Math.round(el.getBoundingClientRect().width)}px`),
      };
    });
    expect(layout).toEqual({
      railWidth: 264,
      mainLeft: 264,
      pageScrollsSideways: false,
      sidebarScrollsSideways: false,
      namesPastTheRail: [],
      squeezedAvatars: [],
    });
  });
});
