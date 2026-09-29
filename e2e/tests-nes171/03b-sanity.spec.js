// Guards the payload the §0.5/§0.6 tests build on. If a VALID task POST stops
// being accepted, every "refused" assertion in those specs would pass for the
// wrong reason, so this runs first and fails loudly instead.
const { test, expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { login, postForm } = require('./helpers');
const { psql } = require('../tests/db');

test('sanity: a fully valid task POST is accepted', async ({ page }) => {
  await login(page, PERSONAS.owner);
  await page.goto('/tasks/new');
  const csrf_token = await page.locator('input[name="csrf_token"]').first().inputValue();
  const ownerId = psql("SELECT id FROM identity.member WHERE display_name = 'Owner A' LIMIT 1;").trim();

  const status = await postForm(page, '/tasks', {
    csrf_token,
    title: 'Sanity probe',
    category: 'chore',
    freq: 'daily',
    interval: '1',
    rotation_policy: 'fixed',
    photo_policy: 'none',
    points: '5',
    lead_time_days: '0',
    pool: ownerId,
  });
  expect(status, 'a valid task must be accepted (303), else the fuzz tests give false passes').toBe(303);
});
