// E2E coverage for the family + chores + points journey (NES-32 / NES-33 /
// gamification):
//
//   1. build a family of four — two adults, two children, each child with login
//      credentials so the suite can sign in as one of them;
//   2. create chores with point values, some assigned to a child and some left
//      unassigned (claimable);
//   3. sign in as the child, complete two assigned chores and claim-then-
//      complete one unassigned chore;
//   4. assert the child's Rewards balance equals the sum of the completed
//      chores' points.
//
// Everything a household owner does here runs through the UI. Only one step is
// seeded: assigned chores materialize their task INSTANCES on a 5-minute
// background scheduler (see cmd/server/main.go taskSchedulerPollInterval), so
// the spec inserts today's instance for the chores it just created rather than
// idling for minutes. Claimable as-needed chores need no seeding — their single
// standing instance is created with the task.
//
// The owner session comes from auth.setup.js via storageState; the child tests
// open their own unauthenticated context and log in.
const { test, expect } = require('@playwright/test');
const { requireEnv } = require('./env');
const { psql } = require('./db');

// One run's fixtures share a timestamp so re-runs never collide with rows left
// behind by an earlier run (display names and chore titles are not unique).
const TS = Date.now();

const CHILD = {
  name: `Mia ${TS}`,
  email: `mia-${TS}@e2e.invalid`,
};
const OTHER_CHILD = { name: `Leo ${TS}` };
const SECOND_ADULT = { name: `Sarah ${TS}` };

// Assigned chores the child completes, plus the unassigned chore the child
// claims. The expected balance is the sum of all three.
const ASSIGNED = [
  { title: `E2E Make bed ${TS}`, points: 5 },
  { title: `E2E Feed the dog ${TS}`, points: 10 },
];
const CLAIMABLE = { title: `E2E Water the plants ${TS}`, points: 7 };
const UNCLAIMED = { title: `E2E Sweep the garage ${TS}`, points: 12 };
const EXPECTED_BALANCE = ASSIGNED[0].points + ASSIGNED[1].points + CLAIMABLE.points;

// The child's password is the suite's own password, so the fixture account is
// never given a credential that is committed to the repository.
const CHILD_PASSWORD = () => requireEnv('NESTOVA_PASSWORD');

const ALL_TITLES = [...ASSIGNED.map((c) => c.title), CLAIMABLE.title, UNCLAIMED.title];

// sqlList renders titles as a SQL IN-list. Titles are spec-owned constants, so
// no user input reaches the statement.
function sqlList(titles) {
  return titles.map((t) => `'${t}'`).join(',');
}

test.afterAll(() => {
  psql(`DELETE FROM recurring_task WHERE title IN (${sqlList(ALL_TITLES)});`);
  // Members live in the shared identity schema; deleting the row takes its
  // credentials with it via the credential table's cascade.
  psql(`DELETE FROM identity.member WHERE display_name IN ('${CHILD.name}','${OTHER_CHILD.name}','${SECOND_ADULT.name}');`);
});

// addMember creates a household member through the add-member form. Passing
// credentials makes the member able to sign in; omitting them creates a
// non-login member.
async function addMember(page, { name, role, email, password }) {
  await page.goto('/members/new');
  await page.locator('input[name="display_name"]').fill(name);
  await page.locator('select[name="role"]').selectOption(role);
  if (email) {
    await page.locator('input[name="email"]').fill(email);
    await page.locator('input[name="password"]').fill(password);
  }
  await page.getByRole('button', { name: 'Add member' }).click();
  await page.waitForURL((u) => new URL(u).pathname === '/', { timeout: 15_000 });
  await expect(page.getByRole('alert')).toHaveCount(0);
}

// localDate returns today's date as YYYY-MM-DD in the browser's own timezone.
// toISOString would render a UTC date, which is the previous or next day for a
// non-UTC user and would anchor the chore on the wrong day.
function localDate() {
  const d = new Date();
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

// addChore fills the cadence builder. An assignee produces a daily, fixed
// rotation chore assigned to that member; no assignee produces an as-needed
// claimable chore, whose standing instance exists as soon as it is saved.
async function addChore(page, { title, points, assignee }) {
  await page.goto('/tasks/new');
  await page.locator('input[name="title"]').fill(title);
  await page.locator('input[name="category"][value="chore"]').check();
  if (assignee) {
    await page.locator('select[name="freq"]').selectOption('daily');
    await page.locator('input[name="interval"]').fill('1');
    await page.locator('input[name="anchor"]').fill(localDate());
    await page.locator('select[name="rotation_policy"]').selectOption('fixed');
    await page.locator('label', { hasText: assignee }).locator('input[name="pool"]').check();
  } else {
    await page.locator('select[name="freq"]').selectOption('as_needed');
  }
  await page.locator('input[name="points"]').fill(String(points));
  await page.getByRole('button', { name: 'Save chore' }).click();
  await page.waitForURL((u) => new URL(u).pathname === '/tasks', { timeout: 15_000 });
  await expect(page.getByRole('alert')).toHaveCount(0);
}

// materializeTodaysInstances inserts the pending instance the background
// scheduler would create for each named chore, assigned to that chore's single
// rotation-pool member. It is idempotent for a given day, so a re-run inside
// the same test session cannot double-seed.
function materializeTodaysInstances(titles) {
  psql(`
    INSERT INTO task_instance (id, household_id, recurring_task_id, assignee_id, due_on, status, kind)
    SELECT gen_random_uuid(), t.household_id, t.id,
           (SELECT rm.member_id FROM rotation_member rm WHERE rm.recurring_task_id = t.id LIMIT 1),
           CURRENT_DATE, 'pending', 'scheduled'
    FROM recurring_task t
    WHERE t.title IN (${sqlList(titles)})
      AND NOT EXISTS (
        SELECT 1 FROM task_instance i
        WHERE i.recurring_task_id = t.id AND i.due_on = CURRENT_DATE
      );
  `);
}

// taskRow finds an instance row by title. Rows live inside the stable
// #task-groups container, whose own id also starts with "task-", so the
// selector is scoped to its descendants. HTMX swaps a row in place
// (outerHTML), so re-query after every action to read the updated row.
function taskRow(page, title) {
  return page.locator('#task-groups [id^="task-"]').filter({ hasText: title }).first();
}

test.describe.serial('Family, chores and points', () => {
  test('an owner can build a family of four (two adults, two children)', async ({ page }) => {
    await addMember(page, { name: SECOND_ADULT.name, role: 'adult' });
    await addMember(page, {
      name: CHILD.name,
      role: 'child',
      email: CHILD.email,
      password: CHILD_PASSWORD(),
    });
    await addMember(page, { name: OTHER_CHILD.name, role: 'child' });

    // All three appear in the dashboard's Family sidebar alongside the owner.
    await page.goto('/');
    const familyHeading = page.getByRole('heading', { name: 'Family' });
    await expect(familyHeading).toBeVisible();
    const familySection = familyHeading.locator('xpath=ancestor::div[1]');
    for (const name of [SECOND_ADULT.name, CHILD.name, OTHER_CHILD.name]) {
      await expect(familySection.getByText(name, { exact: true })).toBeVisible();
    }
  });

  test('an owner can create chores with points, assigned and unassigned', async ({ page }) => {
    for (const chore of ASSIGNED) {
      await addChore(page, { ...chore, assignee: CHILD.name });
    }
    await addChore(page, { ...CLAIMABLE, assignee: null });
    await addChore(page, { ...UNCLAIMED, assignee: null });

    // Persisted point values are what the award later credits, so assert them.
    const rows = psql(
      `SELECT title || '=' || points FROM recurring_task WHERE title IN (${sqlList(ALL_TITLES)}) ORDER BY points;`,
    )
      .trim()
      .split('\n');
    expect(rows).toEqual([
      `${ASSIGNED[0].title}=5`,
      `${CLAIMABLE.title}=7`,
      `${ASSIGNED[1].title}=10`,
      `${UNCLAIMED.title}=12`,
    ]);

    // The claimable chores already have their standing instance; the assigned
    // ones wait on the scheduler, so seed today's instance for them.
    materializeTodaysInstances(ASSIGNED.map((c) => c.title));
  });

  test.describe('as the child', () => {
    // A child signs in with their own credentials, not the owner's storageState.
    test.use({ storageState: { cookies: [], origins: [] } });

    test('completes two assigned chores, claims and completes one unassigned chore, and the balance matches', async ({ page }) => {
      await page.goto('/login');
      await page.fill('input[name="email"]', CHILD.email);
      await page.fill('input[name="password"]', CHILD_PASSWORD());
      await page.click('button:has-text("Sign in")');
      await page.waitForURL((u) => new URL(u).pathname === '/', { timeout: 15_000 });

      await page.goto('/tasks');

      // The two chores assigned to this child complete directly.
      for (const chore of ASSIGNED) {
        const row = taskRow(page, chore.title);
        await expect(row).toBeVisible();
        await row.getByRole('button', { name: 'Done' }).click();
        await expect(taskRow(page, chore.title).getByText('Completed')).toBeVisible();
      }

      // The unassigned chore must be claimed first: claiming assigns it to this
      // child, which is what replaces Claim with Done/Skip on the row.
      const claimable = taskRow(page, CLAIMABLE.title);
      await expect(claimable.getByRole('button', { name: 'Claim' })).toBeVisible();
      await claimable.getByRole('button', { name: 'Claim' }).click();

      const claimed = taskRow(page, CLAIMABLE.title);
      await expect(claimed.getByRole('button', { name: 'Claim' })).toHaveCount(0);
      await claimed.getByRole('button', { name: 'Done' }).click();
      await expect(taskRow(page, CLAIMABLE.title).getByText('Completed')).toBeVisible();

      // The chore nobody claimed stays claimable and must not have been awarded.
      await expect(taskRow(page, UNCLAIMED.title).getByRole('button', { name: 'Claim' })).toBeVisible();

      // The balance equals the sum of the three completed chores' points, and
      // each award appears once in the activity feed.
      await page.goto('/rewards');
      const balanceCard = page
        .locator('div')
        .filter({ has: page.getByRole('heading', { name: 'Your Balance' }) })
        .last();
      await expect(balanceCard).toContainText(`${EXPECTED_BALANCE} pts`);

      const main = page.locator('#main-content');
      for (const chore of [...ASSIGNED, CLAIMABLE]) {
        await expect(main.getByText(`Completed: ${chore.title}`)).toBeVisible();
      }
      await expect(main.getByText(`Completed: ${UNCLAIMED.title}`)).toHaveCount(0);
    });
  });
});
