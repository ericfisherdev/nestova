// Helpers for the §0.2 tenant-isolation, §0.3 role, §0.4 session and §0.8
// HTMX specs.
//
// The tenant-isolation tests need two households acting at once: household A
// creates the object through its own UI routes (so the row is exactly what
// the app would write), and household B then attacks it by id. Each side gets
// its own browser context, so the two sessions never share a cookie jar.
const fs = require('fs');
const path = require('path');
const { expect } = require('@playwright/test');
const { login, csrfToken, postForm } = require('./helpers');
const { psql } = require('../tests/db');

const ZERO_UUID = '00000000-0000-0000-0000-000000000000';

function householdId(name) {
  return psql(`SELECT id FROM identity.household WHERE name = '${name}' LIMIT 1;`).trim();
}

function memberIdByEmail(email) {
  return psql(`SELECT id FROM identity.member WHERE email = '${email}' LIMIT 1;`).trim();
}

// signedIn opens a fresh browser context for persona, so a test can hold two
// households' sessions side by side. The caller closes the context.
async function signedIn(browser, persona) {
  const context = await browser.newContext();
  const page = await context.newPage();
  await login(page, persona);
  return { context, page };
}

// seedInstance inserts a pending chore instance (and its recurring task)
// directly: instances are materialised by a background scheduler, so one
// created through the form has no actionable row inside a test's window.
function seedInstance(household, { assignee = null, title = 'Isolation probe chore', points = 5 } = {}) {
  const taskId = psql(`
    INSERT INTO nestova.recurring_task
      (id, household_id, title, category, cadence, rotation_policy, points, lead_time_days, active, created_at, updated_at)
    VALUES (gen_random_uuid(), '${household}', '${title}', 'chore',
            '{"Freq":"daily","Interval":1,"Anchor":"2026-08-25T00:00:00Z","ByWeekday":null}'::jsonb,
            'claimable', ${points}, 0, true, now(), now())
    RETURNING id;`).trim();
  const who = assignee ? `'${assignee}'` : 'NULL';
  return psql(`
    INSERT INTO nestova.task_instance
      (id, recurring_task_id, household_id, assignee_id, due_on, status, kind, created_at, updated_at)
    VALUES (gen_random_uuid(), '${taskId}', '${household}', ${who}, current_date, 'pending', 'scheduled', now(), now())
    RETURNING id;`).trim();
}

// postPairs posts repeated form keys (recipe ingredient lines), which a plain
// object cannot express. Same redirect mapping as helpers.postForm.
async function postPairs(page, url, pairs) {
  return page.evaluate(async ({ url, pairs }) => {
    const res = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams(pairs).toString(),
      redirect: 'manual',
    });
    return res.type === 'opaqueredirect' ? 303 : res.status;
  }, { url, pairs });
}

// uploadPhoto posts a file through the real multipart form. Playwright's
// request API shares the page's cookies, so the upload is the signed-in
// member's own.
async function uploadPhoto(page, filePath, caption) {
  const token = await csrfToken(page, '/photos');
  const res = await page.request.post('/photos', {
    maxRedirects: 0,
    multipart: {
      csrf_token: token,
      caption,
      photo: { name: path.basename(filePath), mimeType: 'image/png', buffer: fs.readFileSync(filePath) },
    },
  });
  return res.status();
}

// probeForeignId posts the same payload against a foreign id and against an
// id that does not exist. A tenant check that holds must refuse the first
// with a 4xx, and must answer it exactly as it answers the second — any
// difference tells an attacker the foreign id is real (T-0.2.11's rule).
async function probeForeignId(page, pathFor, foreignId, fields) {
  const foreign = await postForm(page, pathFor(foreignId), fields);
  const missing = await postForm(page, pathFor(ZERO_UUID), fields);
  return { foreign, missing };
}

function expectRefusedLikeMissing(label, { foreign, missing }) {
  expect(foreign, `${label}: a foreign id must be refused with a 4xx`).toBeGreaterThanOrEqual(400);
  expect(foreign, `${label}: a foreign id must not produce a server error`).toBeLessThan(500);
  expect(foreign, `${label}: a foreign id must answer exactly like a missing one`).toBe(missing);
}

module.exports = {
  ZERO_UUID,
  householdId,
  memberIdByEmail,
  signedIn,
  seedInstance,
  postPairs,
  uploadPhoto,
  probeForeignId,
  expectRefusedLikeMissing,
};
