const { test, expect } = require('@playwright/test');
const { execSync } = require('child_process');

// Edge case: reward redemption with seeded points (NES-37). The owner is granted
// 50 points and given an affordable (20) and an unaffordable (1000) reward.
// Redeeming the affordable one deducts the cost; the unaffordable one is rejected
// with 409 and leaves the balance unchanged.

function psql(sql) {
  return execSync('docker exec -i nestova-test-db psql -U nestova -d nestova_test -v ON_ERROR_STOP=1 -tA -F"|"', {
    input: sql,
  }).toString().trim();
}

const TS = Date.now();
const cheapName = `Edge Cheap ${TS}`;
const priceyName = `Edge Pricey ${TS}`;
let cheapId;
let priceyId;

// grantSource uniquely tags this spec's point grant so cleanup targets only it.
const grantSource = `edge-${TS}`;

test.beforeAll(() => {
  psql(`INSERT INTO point_ledger (id, household_id, member_id, source_type, points)
        SELECT gen_random_uuid(), household_id, id, '${grantSource}', 50 FROM member WHERE role='owner' LIMIT 1;`);
  const rows = psql(`
    INSERT INTO reward (id, household_id, name, cost_points, active)
    SELECT gen_random_uuid(), (SELECT household_id FROM member WHERE role='owner' LIMIT 1), v.name, v.cost, true
    FROM (VALUES ('${cheapName}', 20), ('${priceyName}', 1000)) AS v(name, cost)
    RETURNING id, name;
  `);
  for (const line of rows.split('\n')) {
    const [id, name] = line.split('|');
    if (!name) continue;
    if (name.includes('Cheap')) cheapId = id.trim();
    if (name.includes('Pricey')) priceyId = id.trim();
  }
});

test.afterAll(() => {
  // Scope cleanup to only this spec's rows: the uniquely-tagged grant, any
  // redemption ledger debits / records pointing at the seeded rewards, and the
  // seeded rewards themselves (delete the referencing rows before the rewards).
  const seededRewards = `(SELECT id FROM reward WHERE name LIKE 'Edge %${TS}')`;
  psql(`DELETE FROM reward_redemption WHERE reward_id IN ${seededRewards};
        DELETE FROM point_ledger WHERE source_type = '${grantSource}' OR source_id IN ${seededRewards};
        DELETE FROM reward WHERE name LIKE 'Edge %${TS}';`);
});

// balanceNow reads the "Your Balance" number. Assertions are relative (before vs
// after) so the suite's shared, accumulating point state can't make them flaky.
async function balanceNow(page) {
  const el = page.locator('p.text-4xl').first();
  await el.waitFor();
  // textContent (not innerText) so it does not depend on layout/visibility timing.
  const m = ((await el.textContent()) || '').match(/\d+/);
  if (!m) throw new Error('Could not parse "Your Balance" value from p.text-4xl');
  return Number(m[0]);
}

test('redeeming an affordable reward deducts the cost from the balance', async ({ page }) => {
  await page.goto('/rewards');
  const before = await balanceNow(page);
  expect(before).toBeGreaterThanOrEqual(20); // we granted 50, so the cheap reward is affordable

  // The cheap reward (cost 20) renders a working Redeem form when affordable.
  await page.locator(`form[action="/rewards/${cheapId}/redeem"]`).getByRole('button', { name: 'Redeem' }).click();

  await page.goto('/rewards');
  expect(await balanceNow(page)).toBe(before - 20);
});

test('redeeming an unaffordable reward is rejected (409) with the balance unchanged', async ({ page }) => {
  await page.goto('/rewards');
  const before = await balanceNow(page);
  expect(before).toBeLessThan(1000); // the pricey reward (cost 1000) is unaffordable

  const csrf = await page.locator('input[name="csrf_token"]').first().inputValue();
  const resp = await page.request.post(`/rewards/${priceyId}/redeem`, {
    form: { csrf_token: csrf },
    maxRedirects: 0,
  });
  expect(resp.status()).toBe(409);
  // The rendered HTML entity-encodes the apostrophe (You don&#39;t…), so match a
  // substring without it.
  expect(await resp.text()).toContain('enough points to redeem this reward');

  await page.goto('/rewards');
  expect(await balanceNow(page)).toBe(before);
});
