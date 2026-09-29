// Helpers shared by the §3 settings and §12 kiosk specs.
//
// Both specs seed members, chores and shopping items in SQL and drive the
// kiosk activation flow; keeping those steps here stops the two files from
// growing divergent copies. Every seeded name carries a per-call suffix so a
// re-run never collides with rows left by an earlier one.
const { expect } = require('@playwright/test');
const { PERSONAS } = require('../tests/fixtures');
const { psql, seedMemberInA } = require('../tests/db');

// unique returns a suffix that differs across calls within one run, even when
// two calls land on the same millisecond.
let counter = 0;
function unique() {
  counter += 1;
  return `${Date.now()}-${counter}`;
}

function householdId(name = 'Household A') {
  return psql(`SELECT id FROM identity.household WHERE name = '${name}' LIMIT 1;`).trim();
}

function memberId(displayName) {
  return psql(`SELECT id FROM identity.member WHERE display_name = '${displayName}' LIMIT 1;`).trim();
}

// seedPersona adds a throwaway member to household A for a test that mutates
// the member irreversibly (a PIN lockout, a phone number, channel
// preferences), so the shared owner/adult/child personas stay untouched.
function seedPersona(label, role = 'adult') {
  const tag = unique();
  const persona = {
    displayName: `${label} ${tag}`,
    email: `${label.toLowerCase().replace(/[^a-z0-9]+/g, '-')}-${tag}@test.local`,
    password: PERSONAS.owner.password,
    role,
  };
  persona.id = seedMemberInA({
    displayName: persona.displayName,
    email: persona.email,
    role,
    copyHashFrom: PERSONAS.owner.email,
  });
  return persona;
}

// localDate is today's date in the machine's own timezone (the server runs on
// the same machine). toISOString would give the UTC date instead.
function localDate(offsetDays = 0) {
  const d = new Date();
  d.setDate(d.getDate() + offsetDays);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

// seedChoreInstance inserts a recurring chore plus today's pending instance.
// Instances are materialised by a background scheduler, so a chore created
// through the form has no actionable row for minutes.
function seedChoreInstance({ household = householdId(), assignee = null, title = `Chore ${unique()}` } = {}) {
  const taskId = psql(`
    INSERT INTO nestova.recurring_task
      (id, household_id, title, category, cadence, rotation_policy, points, lead_time_days, active, created_at, updated_at)
    VALUES (gen_random_uuid(), '${household}', '${title}', 'chore',
            '{"Freq":"daily","Interval":1,"Anchor":"2026-08-25T00:00:00Z","ByWeekday":null}'::jsonb,
            'claimable', 5, 0, true, now(), now())
    RETURNING id;`).trim();
  const who = assignee ? `'${assignee}'` : 'NULL';
  const instanceId = psql(`
    INSERT INTO nestova.task_instance
      (id, recurring_task_id, household_id, assignee_id, due_on, status, kind, created_at, updated_at)
    VALUES (gen_random_uuid(), '${taskId}', '${household}', ${who}, '${localDate()}', 'pending', 'scheduled', now(), now())
    RETURNING id;`).trim();
  return { taskId, instanceId, title };
}

// seedShoppingItem inserts a manual, needed shopping-list item.
function seedShoppingItem({ household = householdId(), name = `Item ${unique()}` } = {}) {
  const id = psql(`
    INSERT INTO nestova.shopping_list_item (id, household_id, name, quantity, unit, source, status, created_at)
    VALUES (gen_random_uuid(), '${household}', '${name}', 1, 'count', 'manual', 'needed', now())
    RETURNING id;`).trim();
  return { id, name };
}

function shoppingStatus(id) {
  return psql(`SELECT status FROM nestova.shopping_list_item WHERE id = '${id}';`).trim();
}

async function settingsToken(page) {
  await page.goto('/settings');
  return page.locator('input[name="csrf_token"]').first().inputValue();
}

// generateCodes asks for n activation codes at once from the signed-in page's
// session and returns each response's status and revealed code. The plaintext
// code exists only in that response (the server keeps a hash), in the
// #kiosk-code-value input, so each body is parsed rather than the database
// read. The requests are fired together, without awaiting between them.
async function generateCodes(page, { name, n = 1 }) {
  const csrf_token = await settingsToken(page);
  return page.evaluate(async ({ csrf_token, name, n }) => {
    const once = async () => {
      const res = await fetch('/settings/kiosk/generate', {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ csrf_token, name }).toString(),
      });
      const doc = new DOMParser().parseFromString(await res.text(), 'text/html');
      const input = doc.querySelector('#kiosk-code-value');
      return { status: res.status, code: input ? input.value : null };
    };
    return Promise.all(Array.from({ length: n }, once));
  }, { csrf_token, name, n });
}

// generateCode returns one freshly revealed activation code, failing the
// calling test if none is revealed.
async function generateCode(page, name) {
  const [{ status, code }] = await generateCodes(page, { name });
  expect(status, 'generating an activation code must succeed').toBe(200);
  expect(code, 'the generate response should reveal an activation code').toBeTruthy();
  return code;
}

// activate redeems code from a page with NO member session — the state a real
// entryway screen is in — and returns the status plus the activation page's
// rendered error text, if any.
async function activate(kiosk, code) {
  await kiosk.goto('/kiosk/activate');
  const csrf_token = await kiosk.locator('input[name="csrf_token"]').first().inputValue();
  return kiosk.evaluate(async ({ csrf_token, code }) => {
    const res = await fetch('/kiosk/activate', {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ csrf_token, code }).toString(),
      redirect: 'manual',
    });
    if (res.type === 'opaqueredirect') return { status: 303, error: '' };
    const doc = new DOMParser().parseFromString(await res.text(), 'text/html');
    const alert = doc.querySelector('[role="alert"]');
    return { status: res.status, error: alert ? alert.textContent.trim() : '' };
  }, { csrf_token, code });
}

// newKiosk opens a member-free browser context and activates it with code,
// failing the calling test if activation is refused.
async function newKiosk(browser, code) {
  const context = await browser.newContext();
  const page = await context.newPage();
  const { status } = await activate(page, code);
  expect(status, 'a valid activation code must be accepted').toBe(303);
  return { context, page };
}

// statusOf fetches a path for its status alone. page.goto() throws on some 4xx
// responses, which would mask exactly the refusals these tests assert.
async function statusOf(page, path) {
  const res = await page.request.get(path, { maxRedirects: 0 });
  return res.status();
}

// postFormBody is helpers.postForm plus the response body, for the tests that
// assert the message a refusal shows and not only its status. headers lets a
// caller send HX-Request to get the fragment an HTMX form would receive.
async function postFormBody(page, path, fields, headers = {}) {
  return page.evaluate(async ({ path, fields, headers }) => {
    const res = await fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded', ...headers },
      body: new URLSearchParams(fields).toString(),
      redirect: 'manual',
    });
    if (res.type === 'opaqueredirect') return { status: 303, body: '' };
    return { status: res.status, body: await res.text() };
  }, { path, fields, headers });
}

// latestDeviceId returns the newest kiosk device of a household, which is the
// one the most recent activation for it created.
function latestDeviceId(household = householdId()) {
  return psql(
    `SELECT id FROM nestova.kiosk_device WHERE household_id = '${household}' ORDER BY created_at DESC LIMIT 1;`,
  ).trim();
}

module.exports = {
  unique,
  householdId,
  memberId,
  seedPersona,
  localDate,
  seedChoreInstance,
  seedShoppingItem,
  shoppingStatus,
  settingsToken,
  generateCodes,
  generateCode,
  activate,
  newKiosk,
  statusOf,
  postFormBody,
  latestDeviceId,
};
