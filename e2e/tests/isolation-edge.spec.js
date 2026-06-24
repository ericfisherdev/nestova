const { test, expect } = require('@playwright/test');
const { execSync } = require('child_process');

// Edge case: tenant isolation. A second household owns a photo; the logged-in
// household must not be able to read it via the raw endpoint or see it listed.
// (NES-75: GET /photos/{id}/raw returns 404 when photo.HouseholdID != member's.)

function psql(sql) {
  return execSync('docker exec -i nestova-test-db psql -U nestova -d nestova_test -v ON_ERROR_STOP=1 -tA', {
    input: sql,
  }).toString().trim();
}

const HH2 = `Edge HH2 ${Date.now()}`;
const HH2_CAPTION = `HH2 secret caption ${Date.now()}`;
let hh2PhotoId;

test.beforeAll(() => {
  hh2PhotoId = psql(`
    WITH hh AS (INSERT INTO household (name) VALUES ('${HH2}') RETURNING id),
         mem AS (INSERT INTO member (id, household_id, display_name, role, color_key)
                 SELECT gen_random_uuid(), id, 'HH2 Owner', 'owner', 'clay' FROM hh RETURNING id, household_id)
    INSERT INTO photo (id, household_id, storage_ref, caption, uploaded_by)
    SELECT gen_random_uuid(), m.household_id, 'edge/secret.jpg', '${HH2_CAPTION}', m.id FROM mem m
    RETURNING id;
  `).split('\n')[0].trim();
});

test.afterAll(() => {
  psql(`DELETE FROM household WHERE name = '${HH2}';`);
});

test("another household's photo cannot be fetched via /photos/{id}/raw (404)", async ({ page }) => {
  const resp = await page.request.get(`/photos/${hh2PhotoId}/raw`, { maxRedirects: 0 });
  expect(resp.status()).toBe(404);
});

test("another household's photo does not leak into the photos list", async ({ page }) => {
  await page.goto('/photos');
  await expect(page.getByText(HH2_CAPTION)).toHaveCount(0);
});
