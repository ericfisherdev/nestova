// E2E coverage for the chore PIN gate (NES-166): a member with a PIN
// enrolled must enter it to complete or skip a chore assigned to them, so one
// child cannot take the points for another child's chore on the shared
// entryway screen.
//
// The journey, all through the UI except the one seeded step every chore spec
// shares (assigned chores materialize their task INSTANCES on a 5-minute
// background scheduler, so the spec inserts today's instance rather than
// idling for minutes):
//
//   1. the owner builds two children with login credentials and one chore for
//      each, then sets both children's PINs from /settings;
//   2. signed in as the second child, the first child's chore row demands a
//      PIN and refuses a wrong one — the row stays actionable and no points
//      move;
//   3. the same child completes their OWN chore with their OWN PIN and the
//      points land in their balance;
//   4. a third child with no PIN enrolled sees no PIN field at all and
//      completes exactly as before the gate existed.
const { test, expect } = require("@playwright/test");
const { requireEnv } = require("./env");
const { psql } = require("./db");

// One run's fixtures share a timestamp so re-runs never collide with rows left
// behind by an earlier run (display names and chore titles are not unique).
const TS = Date.now();

const GATED_CHILD = {
  name: `Pia ${TS}`,
  email: `pia-${TS}@e2e.invalid`,
  pin: "4821",
};
const OTHER_CHILD = {
  name: `Nils ${TS}`,
  email: `nils-${TS}@e2e.invalid`,
  pin: "1357",
};
const UNENROLLED_CHILD = { name: `Ove ${TS}`, email: `ove-${TS}@e2e.invalid` };

const CHORES = {
  gated: { title: `E2E PIN gated ${TS}`, points: 9, member: GATED_CHILD },
  own: { title: `E2E PIN own ${TS}`, points: 4, member: OTHER_CHILD },
  unenrolled: {
    title: `E2E PIN unenrolled ${TS}`,
    points: 3,
    member: UNENROLLED_CHILD,
  },
};
const ALL_TITLES = Object.values(CHORES).map((c) => c.title);

// Fixture children sign in with the suite's own password, so no fixture
// account is given a credential committed to the repository.
const CHILD_PASSWORD = () => requireEnv("NESTOVA_PASSWORD");

function sqlList(values) {
  return values.map((v) => `'${v}'`).join(",");
}

test.afterAll(() => {
  psql(`DELETE FROM recurring_task WHERE title IN (${sqlList(ALL_TITLES)});`);
  // Members live in the shared identity schema; deleting the row takes its
  // credentials and its PIN with it via their cascades.
  psql(
    `DELETE FROM identity.member WHERE display_name IN (${sqlList([
      GATED_CHILD.name,
      OTHER_CHILD.name,
      UNENROLLED_CHILD.name,
    ])});`,
  );
});

async function addMember(page, { name, email, password }) {
  await page.goto("/members/new");
  await page.locator('input[name="display_name"]').fill(name);
  await page.locator('select[name="role"]').selectOption("child");
  await page.locator('input[name="email"]').fill(email);
  await page.locator('input[name="password"]').fill(password);
  await page.getByRole("button", { name: "Add member" }).click();
  await page.waitForURL((u) => new URL(u).pathname === "/", {
    timeout: 15_000,
  });
  await expect(page.getByRole("alert")).toHaveCount(0);
}

// localDate returns today's date as YYYY-MM-DD in the browser's own timezone.
// toISOString would render a UTC date, which is the previous or next day for a
// non-UTC user and would anchor the chore on the wrong day.
function localDate() {
  const d = new Date();
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}

async function addChore(page, { title, points, assignee }) {
  await page.goto("/tasks/new");
  await page.locator('input[name="title"]').fill(title);
  await page.locator('input[name="category"][value="chore"]').check();
  await page.locator('select[name="freq"]').selectOption("daily");
  await page.locator('input[name="interval"]').fill("1");
  await page.locator('input[name="anchor"]').fill(localDate());
  await page.locator('select[name="rotation_policy"]').selectOption("fixed");
  await page
    .locator("label", { hasText: assignee })
    .locator('input[name="pool"]')
    .check();
  await page.locator('input[name="points"]').fill(String(points));
  await page.getByRole("button", { name: "Save chore" }).click();
  await page.waitForURL((u) => new URL(u).pathname === "/tasks", {
    timeout: 15_000,
  });
  await expect(page.getByRole("alert")).toHaveCount(0);
}

// materializeTodaysInstances inserts the pending instance the background
// scheduler would create for each named chore, assigned to that chore's single
// rotation-pool member. Idempotent for a given day.
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

// setMemberPIN drives the owner-only "Set a family member's PIN" card on
// /settings — the same affordance a parent uses for a child who cannot set
// their own.
async function setMemberPIN(page, { name, pin }) {
  await page.goto("/settings");
  const row = page.locator("li").filter({ hasText: name }).first();
  await row.locator('input[name="pin"]').fill(pin);
  await row.getByRole("button", { name: /^(Set|Change)$/ }).click();
  await page.waitForURL((u) => new URL(u).pathname === "/settings", {
    timeout: 15_000,
  });
  await expect(
    page.locator("li").filter({ hasText: name }).first(),
  ).toContainText("PIN set");
}

// taskRow finds an instance row by title. HTMX swaps a row in place
// (outerHTML), so re-query after every action to read the updated row.
function taskRow(page, title) {
  return page
    .locator('#task-groups [id^="task-"]')
    .filter({ hasText: title })
    .first();
}

async function signIn(page, { email }) {
  await page.goto("/login");
  await page.fill('input[name="email"]', email);
  await page.fill('input[name="password"]', CHILD_PASSWORD());
  await page.click('button:has-text("Sign in")');
  await page.waitForURL((u) => new URL(u).pathname === "/", {
    timeout: 15_000,
  });
}

test.describe.serial("Chore PIN gate", () => {
  test("an owner builds the family, their chores, and two of the three PINs", async ({
    page,
  }) => {
    for (const child of [GATED_CHILD, OTHER_CHILD, UNENROLLED_CHILD]) {
      await addMember(page, {
        name: child.name,
        email: child.email,
        password: CHILD_PASSWORD(),
      });
    }
    for (const chore of Object.values(CHORES)) {
      await addChore(page, {
        title: chore.title,
        points: chore.points,
        assignee: chore.member.name,
      });
    }
    materializeTodaysInstances(ALL_TITLES);

    await setMemberPIN(page, GATED_CHILD);
    await setMemberPIN(page, OTHER_CHILD);
  });

  test.describe("as a child with a PIN", () => {
    // A child signs in with their own credentials, not the owner's storageState.
    test.use({ storageState: { cookies: [], origins: [] } });

    test("cannot complete another child’s chore, but completes their own with their PIN", async ({
      page,
    }) => {
      await signIn(page, OTHER_CHILD);
      await page.goto("/tasks");

      // Another child's chore is gated: it shows a PIN field, and this child's
      // own PIN does not open it.
      const gated = taskRow(page, CHORES.gated.title);
      await expect(
        gated.locator('[data-testid="task-pin-input"]'),
      ).toBeVisible();
      await gated
        .locator('[data-testid="task-pin-input"]')
        .fill(OTHER_CHILD.pin);
      await gated.getByRole("button", { name: "Done" }).click();

      const refused = taskRow(page, CHORES.gated.title);
      await expect(
        refused.locator('[data-testid="task-pin-error"]'),
      ).toBeVisible();
      // The error must not disclose whether that member is enrolled.
      await expect(
        refused.locator('[data-testid="task-pin-error"]'),
      ).toContainText("That PIN could not be verified.");
      // The row is untouched: still actionable, never marked complete.
      await expect(refused.getByRole("button", { name: "Done" })).toBeVisible();
      await expect(refused.getByText("Completed")).toHaveCount(0);

      // Skipping is gated identically.
      await refused.locator('[data-testid="task-pin-input"]').fill("0000");
      await refused.getByRole("button", { name: "Skip" }).click();
      await expect(
        taskRow(page, CHORES.gated.title).getByText("Skipped"),
      ).toHaveCount(0);

      // Their OWN chore completes with their OWN PIN.
      const own = taskRow(page, CHORES.own.title);
      await own.locator('[data-testid="task-pin-input"]').fill(OTHER_CHILD.pin);
      await own.getByRole("button", { name: "Done" }).click();
      await expect(
        taskRow(page, CHORES.own.title).getByText("Completed"),
      ).toBeVisible();

      // The points landed with this child, and only for their own chore.
      await page.goto("/rewards");
      const balanceCard = page
        .locator("div")
        .filter({ has: page.getByRole("heading", { name: "Your Balance" }) })
        .last();
      await expect(balanceCard).toContainText(`${CHORES.own.points} pts`);

      const main = page.locator("#main-content");
      await expect(
        main.getByText(`Completed: ${CHORES.own.title}`),
      ).toBeVisible();
      await expect(
        main.getByText(`Completed: ${CHORES.gated.title}`),
      ).toHaveCount(0);
    });
  });

  test.describe("as a child with no PIN", () => {
    test.use({ storageState: { cookies: [], origins: [] } });

    test("sees no PIN field and completes their chore exactly as before", async ({
      page,
    }) => {
      await signIn(page, UNENROLLED_CHILD);
      await page.goto("/tasks");

      const row = taskRow(page, CHORES.unenrolled.title);
      await expect(row).toBeVisible();
      await expect(row.locator('[data-testid="task-pin-input"]')).toHaveCount(
        0,
      );

      await row.getByRole("button", { name: "Done" }).click();
      await expect(
        taskRow(page, CHORES.unenrolled.title).getByText("Completed"),
      ).toBeVisible();
    });
  });
});
