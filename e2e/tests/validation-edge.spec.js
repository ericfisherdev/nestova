const { test, expect } = require('@playwright/test');
const { execSync } = require('child_process');

// Edge case: server-side validation boundaries. Several forms carry client-side
// constraints (min/step), so we POST directly with a real CSRF token to make
// sure the SERVER rejects bad input. A rejected mutation re-renders (422) rather
// than 303-redirecting; with maxRedirects:0 a 303 means the value was accepted.

function psql(sql) {
  return execSync('docker exec -i nestova-test-db psql -U nestova -d nestova_test -v ON_ERROR_STOP=1 -tA -q', {
    input: sql,
  }).toString().trim();
}

// csrf navigates to a page carrying the action's form and returns its token.
async function csrf(page, path) {
  await page.goto(path);
  return page.locator('input[name="csrf_token"]').first().inputValue();
}

const TS = Date.now();

test('a subscription with a negative amount is rejected (not created)', async ({ page }) => {
  const token = await csrf(page, '/subscriptions');
  const name = `Neg ${TS}`;
  const resp = await page.request.post('/subscriptions', {
    form: { csrf_token: token, name, amount: '-5.00', currency: 'USD', cycle: 'monthly', next_renewal_on: '2030-01-01' },
    maxRedirects: 0,
  });
  expect(resp.status(), 'negative amount must be rejected, not a 303 success').toBeGreaterThanOrEqual(400);
  await page.goto('/subscriptions');
  await expect(page.getByText(name, { exact: true })).toHaveCount(0);
});

test('a recipe with zero servings is rejected', async ({ page }) => {
  const token = await csrf(page, '/meals');
  const resp = await page.request.post('/meals/recipes', {
    form: {
      csrf_token: token,
      title: `ZeroServe ${TS}`,
      servings: '0',
      instructions: 'x',
      ingredient_name: 'flour',
      ingredient_amount: '1',
      ingredient_unit: 'count',
    },
    maxRedirects: 0,
  });
  expect(resp.status(), 'zero servings must be rejected').toBeGreaterThanOrEqual(400);
});

test('a recurring task with a negative interval is rejected', async ({ page }) => {
  const token = await csrf(page, '/tasks/new');
  const resp = await page.request.post('/tasks', {
    form: {
      csrf_token: token,
      title: `NegInt ${TS}`,
      category: 'chore',
      freq: 'daily',
      interval: '-1',
      rotation_policy: 'claimable',
      points: '0',
      lead_time_days: '0',
    },
    maxRedirects: 0,
  });
  expect(resp.status(), 'negative interval must be rejected').toBeGreaterThanOrEqual(400);
});

test('consuming more than on-hand never drives the pantry quantity negative', async ({ page }) => {
  const itemId = psql(`
    WITH ing AS (
      INSERT INTO ingredient (id, canonical_name) VALUES (gen_random_uuid(), 'edge-underflow-${TS}') RETURNING id
    )
    INSERT INTO pantry_item (id, household_id, ingredient_id, quantity, unit)
    SELECT gen_random_uuid(), (SELECT household_id FROM member WHERE role='owner' LIMIT 1), ing.id, 2, 'count' FROM ing
    RETURNING id;
  `).trim();
  try {
    const token = await csrf(page, '/groceries');
    await page.request.post(`/groceries/pantry/${itemId}/consume`, {
      form: { csrf_token: token, amount: '5', unit: 'count' },
      maxRedirects: 0,
    });
    const qty = psql(`SELECT quantity FROM pantry_item WHERE id = '${itemId}';`).trim();
    // The item may be clamped to 0 or removed, but it must never go negative.
    if (qty !== '') {
      expect(Number(qty), `pantry quantity after over-consume = ${qty}`).toBeGreaterThanOrEqual(0);
    }
  } finally {
    // Remove the pantry row before its ingredient (FK references ingredient).
    psql(`DELETE FROM pantry_item WHERE id = '${itemId}';
          DELETE FROM ingredient WHERE canonical_name = 'edge-underflow-${TS}';`);
  }
});
