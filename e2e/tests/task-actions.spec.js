const { test, expect } = require('@playwright/test');
const { execSync } = require('child_process');

// E2E coverage for the task instance actions (NES-32): complete / skip / claim.
//
// Task instances are normally materialized by a 5-minute background scheduler,
// so the create flow alone never shows an actionable row in a test window. We
// seed pending instances directly via SQL, then exercise the actions through the
// UI. A MONTHLY cadence anchored today keeps the next occurrence beyond the
// 14-day generation horizon, so the scheduler does not multiply these rows; the
// recurring task is left active=true so the list join renders its title.

const TS = Date.now();
const titles = {
  done: `E2E Done ${TS}`,
  skip: `E2E Skip ${TS}`,
  claim: `E2E Claim ${TS}`,
};

function psql(sql) {
  return execSync('docker exec -i nestova-test-db psql -U nestova -d nestova_test -v ON_ERROR_STOP=1 -q', {
    input: sql,
  }).toString();
}

test.beforeAll(() => {
  psql(`
    WITH ins_rt AS (
      INSERT INTO recurring_task (id, household_id, title, category, cadence, rotation_policy, points, lead_time_days, active)
      SELECT gen_random_uuid(), h.id, t.title, 'chore',
             ('{"Freq":"monthly","Anchor":"' || to_char(CURRENT_DATE, 'YYYY-MM-DD') || 'T00:00:00Z","Interval":1,"ByWeekday":null}')::jsonb,
             t.policy, 5, 0, true
      FROM household h
      CROSS JOIN (VALUES
        ('${titles.done}','fixed'),
        ('${titles.skip}','fixed'),
        ('${titles.claim}','claimable')
      ) AS t(title, policy)
      RETURNING id, rotation_policy
    )
    INSERT INTO task_instance (id, household_id, recurring_task_id, assignee_id, due_on, status)
    SELECT gen_random_uuid(), (SELECT id FROM household LIMIT 1), r.id,
           CASE WHEN r.rotation_policy = 'claimable' THEN NULL
                ELSE (SELECT id FROM member WHERE role = 'owner' LIMIT 1) END,
           CURRENT_DATE, 'pending'
    FROM ins_rt r;
  `);
});

test.afterAll(() => {
  psql(`DELETE FROM recurring_task WHERE title IN ('${titles.done}','${titles.skip}','${titles.claim}');`);
});

// taskRow finds the in-list instance row carrying a given title. The HTMX
// actions swap this row (#task-{id}, outerHTML) in place, so re-querying after
// an action returns the updated row.
function taskRow(page, title) {
  return page.locator('[id^="task-"]').filter({ hasText: title }).first();
}

test('completing a task marks the row Completed', async ({ page }) => {
  await page.goto('/tasks');
  const row = taskRow(page, titles.done);
  await expect(row).toBeVisible();
  await row.getByRole('button', { name: 'Done' }).click();
  await expect(taskRow(page, titles.done).getByText('Completed')).toBeVisible();
});

test('skipping a task marks the row Skipped', async ({ page }) => {
  await page.goto('/tasks');
  const row = taskRow(page, titles.skip);
  await expect(row).toBeVisible();
  await row.getByRole('button', { name: 'Skip' }).click();
  await expect(taskRow(page, titles.skip).getByText('Skipped')).toBeVisible();
});

test('claiming an unassigned task assigns it (Claim becomes Done/Skip)', async ({ page }) => {
  await page.goto('/tasks');
  const row = taskRow(page, titles.claim);
  await expect(row).toBeVisible();
  await expect(row.getByRole('button', { name: 'Claim' })).toBeVisible();

  await row.getByRole('button', { name: 'Claim' }).click();

  // After claiming, the instance is assigned to the current member, so the row
  // exposes complete/skip instead of claim.
  const claimed = taskRow(page, titles.claim);
  await expect(claimed.getByRole('button', { name: 'Done' })).toBeVisible();
  await expect(claimed.getByRole('button', { name: 'Claim' })).toHaveCount(0);
});
