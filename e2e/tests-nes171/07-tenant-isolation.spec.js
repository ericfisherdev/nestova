// §0.2 tenant isolation (IDOR) — the highest-value section in the checklist.
//
// Household B is SQL-seeded (see tests/db.js) because Nestova provisions a
// single household through the UI. The schema is household-scoped everywhere,
// and NES-165 was exactly this bug class — a cross-household write that the
// composite FK did not catch — so these are worth running even though the
// product does not expose multi-household onboarding.
const { test, expect } = require('@playwright/test');
const { PERSONAS, PASSWORD } = require('../tests/fixtures');
const { login, postForm } = require('./helpers');
const { psql, seedHouseholdB } = require('../tests/db');

const OWNER_B = { email: 'owner@other.local', password: PASSWORD, displayName: 'Owner B' };

let b;

test.beforeAll(() => {
  b = seedHouseholdB({
    householdName: 'Household B',
    ownerName: OWNER_B.displayName,
    ownerEmail: OWNER_B.email,
    copyHashFrom: PERSONAS.owner.email,
  });
});

// Creates a chore in household A and returns its instance id, so B can try to
// act on it. Seeded directly: instances are materialised by a background
// scheduler, so a chore created through the form has no actionable row yet.
function seedInstanceInA() {
  const householdA = psql(
    "SELECT id FROM identity.household WHERE name = 'Household A' LIMIT 1;",
  ).trim();
  const ownerA = psql(
    `SELECT id FROM identity.member WHERE household_id = '${householdA}' AND role = 'owner' LIMIT 1;`,
  ).trim();

  const taskId = psql(`
    INSERT INTO nestova.recurring_task
      (id, household_id, title, category, cadence, rotation_policy, points, lead_time_days, active, created_at, updated_at)
    VALUES (gen_random_uuid(), '${householdA}', 'Isolation probe chore', 'chore',
            '{"Freq":"daily","Interval":1,"Anchor":"2026-08-25T00:00:00Z","ByWeekday":null}'::jsonb,
            'claimable', 5, 0, true, now(), now())
    RETURNING id;
  `).trim();

  const instanceId = psql(`
    INSERT INTO nestova.task_instance
      (id, recurring_task_id, household_id, assignee_id, due_on, status, kind, created_at, updated_at)
    VALUES (gen_random_uuid(), '${taskId}', '${householdA}', '${ownerA}', current_date, 'pending', 'scheduled', now(), now())
    RETURNING id;
  `).trim();

  return { householdA, ownerA, taskId, instanceId };
}

test.describe('§0.2 tenant isolation', () => {
  test('sanity: the seeded household B owner can sign in', async ({ page }) => {
    await login(page, OWNER_B);
    expect(b.householdId, 'household B should have been seeded').toBeTruthy();
  });

  test('T-0.2.1 household B cannot act on household A chores', async ({ page }) => {
    const a = seedInstanceInA();
    await login(page, OWNER_B);
    await page.goto('/tasks');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();

    const results = {};
    for (const action of ['complete', 'skip', 'claim']) {
      results[action] = await postForm(page, `/tasks/${a.instanceId}/${action}`, { csrf_token });
    }
    const leaked = Object.entries(results).filter(([, s]) => s < 400);
    expect(leaked, "household B actions on A's instance that were not refused").toEqual([]);

    const status = psql(
      `SELECT status FROM nestova.task_instance WHERE id = '${a.instanceId}';`,
    ).trim();
    expect(status, "A's instance must be untouched").toBe('pending');
  });

  test('T-0.2.9 household B cannot set a PIN on a household A member', async ({ page }) => {
    const a = seedInstanceInA();
    await login(page, OWNER_B);
    await page.goto('/settings');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();

    const status = await postForm(page, `/settings/members/${a.ownerA}/pin`, { csrf_token, pin: '9999' });
    expect(status, "B must not set a PIN on A's member").toBeGreaterThanOrEqual(400);

    const pins = psql(
      `SELECT count(*) FROM identity.member_pin WHERE member_id = '${a.ownerA}';`,
    ).trim();
    expect(pins, "no PIN row may exist for A's member").toBe('0');
  });

  test('T-0.2.11 a foreign id is indistinguishable from a missing one', async ({ page }) => {
    const a = seedInstanceInA();
    await login(page, OWNER_B);
    await page.goto('/tasks');
    const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();

    const foreign = await postForm(page, `/tasks/${a.instanceId}/complete`, { csrf_token });
    const missing = await postForm(page, '/tasks/00000000-0000-0000-0000-000000000000/complete', { csrf_token });

    expect({ foreign, missing },
      'a foreign id and an unknown id must answer identically, or the response enumerates ids',
    ).toEqual({ foreign: missing, missing });
  });
});
