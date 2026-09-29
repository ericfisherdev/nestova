// Chore-specific helpers for the NES-171 §5 specs (creation, claims, and the
// §5.2 in-place actions).
//
// Task instances are materialised by a background scheduler that ticks every
// 5 minutes (cmd/server/main.go taskSchedulerPollInterval), so specs seed
// instance and claim rows with psql and keep every assertion on HTTP, the UI,
// or a persisted row.
//
// Tests act as FRESH members seeded per test (newMember) rather than the shared
// owner/adult/child personas wherever an assignee, a balance, or a PIN is
// involved: another spec may enrol a persona's PIN (NES-166 gates completing
// and skipping on it) or move their balance, and a fresh member makes both
// start from a known state.
const { PASSWORD, PERSONAS } = require('../tests/fixtures');
const { psql, seedMemberInA } = require('../tests/db');
const { login } = require('./helpers');
const { randomInt } = require('node:crypto');

// sqlText quotes s as a SQL string literal.
function sqlText(s) {
  return `'${String(s).replace(/'/g, "''")}'`;
}

function householdA() {
  return psql("SELECT id FROM identity.household WHERE name = 'Household A' LIMIT 1;").trim();
}

function uniqueSuffix() {
  return `${Date.now()}${randomInt(1000)}`;
}

// newMember seeds a member of household A who can sign in with the shared
// fixture password, has no PIN, and has an empty point balance.
function newMember(label, role = 'adult') {
  const suffix = uniqueSuffix();
  const displayName = `${label} ${suffix}`;
  const email = `${label.toLowerCase().replace(/[^a-z0-9]+/g, '-')}-${suffix}@test.local`;
  const id = seedMemberInA({ displayName, email, role, copyHashFrom: PERSONAS.owner.email });
  return { id, displayName, email, password: PASSWORD };
}

function signIn(page, member) {
  return login(page, { email: member.email, password: member.password });
}

// todayISO is today's UTC date. The app stamps due dates with domain.DateOf,
// which is UTC, so a UTC date is the one the server compares against.
function todayISO() {
  return new Date().toISOString().slice(0, 10);
}

// daysFromTodayISO returns the UTC date `days` away from today.
function daysFromTodayISO(days) {
  const d = new Date();
  d.setUTCDate(d.getUTCDate() + days);
  return d.toISOString().slice(0, 10);
}

// seedTask inserts an active recurring task. The cadence is daily and anchored
// today unless overridden; seeded instances do not depend on it.
function seedTask({ title = `Chore ${uniqueSuffix()}`, policy = 'claimable', points = 5 } = {}) {
  const cadence = JSON.stringify({ Freq: 'daily', Interval: 1, Anchor: `${todayISO()}T00:00:00Z`, ByWeekday: null });
  return psql(`
    INSERT INTO nestova.recurring_task
      (id, household_id, title, category, cadence, rotation_policy, points, lead_time_days, active, created_at, updated_at)
    VALUES (gen_random_uuid(), '${householdA()}', ${sqlText(title)}, 'chore',
            ${sqlText(cadence)}::jsonb, '${policy}', ${points}, 0, true, now(), now())
    RETURNING id;`).trim();
}

// seedInstance inserts today's pending instance of a fresh recurring task and
// returns { id, taskId, title }. assignee null leaves it "Up for grabs".
function seedInstance({ assignee = null, title, points = 5, policy } = {}) {
  const taskTitle = title || `Chore ${uniqueSuffix()}`;
  const taskPolicy = policy || (assignee ? 'fixed' : 'claimable');
  const taskId = seedTask({ title: taskTitle, points, policy: taskPolicy });
  if (taskPolicy !== 'claimable') {
    // A fixed task needs its pool, or every scheduler tick logs
    // ErrNoRotationMembers for it.
    psql(`
      INSERT INTO nestova.rotation_member (household_id, recurring_task_id, member_id, position)
      VALUES ('${householdA()}', '${taskId}', '${assignee}', 0);`);
  }
  const who = assignee ? `'${assignee}'` : 'NULL';
  const id = psql(`
    INSERT INTO nestova.task_instance
      (id, recurring_task_id, household_id, assignee_id, due_on, status, kind, created_at, updated_at)
    VALUES (gen_random_uuid(), '${taskId}', '${householdA()}', ${who}, current_date, 'pending', 'scheduled', now(), now())
    RETURNING id;`).trim();
  return { id, taskId, title: taskTitle };
}

// seedClaim seeds an instance that `claimant` claimed off "Up for grabs", so it
// carries an at-risk claim window. expiresIn is a SQL interval relative to now
// ('-1 minute' is a claim that lapsed a minute ago); claimed_at is back-dated to
// exactly one ClaimWindow (12h) before the expiry, as Claim would have stamped it.
function seedClaim({ claimant, expiresIn, points = 10, title, expiresAt } = {}) {
  const inst = seedInstance({ title, points, policy: 'claimable' });
  const expiry = expiresAt ? `${sqlText(expiresAt)}::timestamptz` : `now() + interval ${sqlText(expiresIn)}`;
  psql(`
    UPDATE nestova.task_instance
       SET assignee_id = '${claimant}', claimed_by = '${claimant}',
           claim_expires_at = ${expiry},
           claimed_at = ${expiry} - interval '12 hours'
     WHERE id = '${inst.id}';`);
  return inst;
}

function instanceRow(id) {
  const [status, assignee, claimedBy, expires, warned] = psql(`
    SELECT status, coalesce(assignee_id::text, ''), coalesce(claimed_by::text, ''),
           coalesce(claim_expires_at::text, ''), coalesce(claim_warned_at::text, '')
      FROM nestova.task_instance WHERE id = '${id}';`).trim().split('|');
  return { status, assignee, claimedBy, expires, warned };
}

// ledgerFor returns the point_ledger rows recorded against an instance.
function ledgerFor(instanceId) {
  const out = psql(`
    SELECT member_id, source_type, points FROM nestova.point_ledger
     WHERE source_id = '${instanceId}' ORDER BY created_at;`).trim();
  return out ? out.split('\n').map((line) => {
    const [memberId, sourceType, points] = line.split('|');
    return { memberId, sourceType, points: Number(points) };
  }) : [];
}

function balanceOf(memberId) {
  return Number(psql(
    `SELECT coalesce(sum(points), 0) FROM nestova.point_ledger WHERE member_id = '${memberId}';`,
  ).trim());
}

async function tasksToken(page) {
  await page.goto('/tasks');
  return page.locator('input[name="csrf_token"]').first().inputValue();
}

// taskRow finds an instance row by id. HTMX swaps a row in place (outerHTML)
// under the same id, so the locator stays valid across an action.
function taskRow(page, instanceId) {
  return page.locator(`#task-${instanceId}`);
}

// postPairs POSTs an urlencoded form built from [name, value] pairs (repeated
// names allowed — the create form sends byweekday and pool that way) from
// inside the browser session, and returns { status, body }. A same-origin
// redirect is reported as 303, as helpers.postForm does.
async function postPairs(page, path, pairs) {
  return page.evaluate(async ({ path, pairs }) => {
    const res = await fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams(pairs).toString(),
      redirect: 'manual',
    });
    if (res.type === 'opaqueredirect') return { status: 303, body: '' };
    return { status: res.status, body: await res.text() };
  }, { path, pairs });
}

// choreForm builds a POST /tasks payload for a claimable daily chore anchored
// today — a payload the handler accepts — with fields overridden or added.
// Array-valued overrides (byweekday, pool) become repeated fields; a null
// override drops the field.
function choreForm(csrfToken, overrides = {}) {
  const fields = {
    csrf_token: csrfToken,
    title: `Chore ${uniqueSuffix()}`,
    category: 'chore',
    freq: 'daily',
    interval: '1',
    anchor: todayISO(),
    rotation_policy: 'claimable',
    photo_policy: 'none',
    points: '5',
    lead_time_days: '0',
    ...overrides,
  };
  const pairs = [];
  for (const [name, value] of Object.entries(fields)) {
    if (value === null || value === undefined) continue;
    for (const v of Array.isArray(value) ? value : [value]) pairs.push([name, String(v)]);
  }
  return pairs;
}

function titleOf(pairs) {
  return pairs.find(([name]) => name === 'title')[1];
}

// storedTask returns the persisted recurring task for a title, or null.
function storedTask(title) {
  const out = psql(`
    SELECT id, rotation_policy, cadence::text FROM nestova.recurring_task
     WHERE title = ${sqlText(title)} LIMIT 1;`).trim();
  if (!out) return null;
  const [id, policy, cadence] = out.split('|');
  return { id, policy, cadence: JSON.parse(cadence) };
}

module.exports = {
  sqlText,
  householdA,
  uniqueSuffix,
  newMember,
  signIn,
  todayISO,
  daysFromTodayISO,
  seedTask,
  seedInstance,
  seedClaim,
  instanceRow,
  ledgerFor,
  balanceOf,
  tasksToken,
  taskRow,
  postPairs,
  choreForm,
  titleOf,
  storedTask,
};
