// Shared psql helper for specs that seed or inspect fixture rows directly.
//
// Some flows cannot be driven end to end through the UI inside a test window:
// task INSTANCES are materialized by a 5-minute background scheduler, so a
// chore created through the form has no actionable row for minutes. Specs seed
// those rows themselves and keep every assertion on the UI.
//
// The container and database default to the values the existing specs hardcode
// and are overridable so the suite can also run against an ad-hoc test
// instance. search_path is pinned because Nestova's tables live in the
// "nestova" schema (NSTR-118), not public.
const { execFileSync } = require('child_process');

const CONTAINER = process.env.NESTOVA_E2E_PG_CONTAINER || 'nestova-test-db';
const DATABASE = process.env.NESTOVA_E2E_PG_DB || 'nestova_test';
const USER = process.env.NESTOVA_E2E_PG_USER || 'nestova';

// psql runs sql inside the test database and returns stdout. ON_ERROR_STOP
// turns any SQL error into a non-zero exit, which fails the calling test rather
// than silently seeding nothing.
function psql(sql) {
  return execFileSync(
    'docker',
    ['exec', '-i', CONTAINER, 'psql', '-U', USER, '-d', DATABASE, '-v', 'ON_ERROR_STOP=1', '-q', '-At'],
    { input: `SET search_path TO nestova, identity, public;\n${sql}` },
  ).toString();
}


// seedHouseholdB creates a SECOND household with its own owner, directly in
// SQL, and returns their ids.
//
// SQL rather than the UI because Nestova provisions a single household
// (household.ErrHouseholdExists) — there is no supported path to a second one
// through onboarding. The schema is household-scoped throughout, so the
// tenant-isolation tests it enables are still meaningful; only their setup has
// to bypass the UI.
//
// The password hash is copied from household A's owner so the seeded owner can
// actually sign in with the shared fixture password, rather than trying to
// reproduce the app's argon2id parameters here.
function seedHouseholdB({ householdName, ownerName, ownerEmail, copyHashFrom }) {
  const existing = psql(
    `SELECT h.id || ',' || m.id FROM identity.household h
       JOIN identity.member m ON m.household_id = h.id
      WHERE h.name = '${householdName}' LIMIT 1;`,
  ).trim();
  if (existing) {
    const [householdId, ownerId] = existing.split(',');
    return { householdId, ownerId };
  }

  const out = psql(`
    WITH new_household AS (
      INSERT INTO identity.household (id, name, created_at, updated_at)
      VALUES (gen_random_uuid(), '${householdName}', now(), now())
      RETURNING id
    ), source AS (
      SELECT password_hash FROM identity.member WHERE email = '${copyHashFrom}' LIMIT 1
    ), new_member AS (
      INSERT INTO identity.member
        (id, household_id, display_name, role, email, password_hash, active, created_at, updated_at)
      SELECT gen_random_uuid(), new_household.id, '${ownerName}', 'owner',
             '${ownerEmail}', source.password_hash, true, now(), now()
      FROM new_household, source
      RETURNING id, household_id
    )
    SELECT household_id || ',' || id FROM new_member;
  `).trim();

  const [householdId, ownerId] = out.split(',');
  return { householdId, ownerId };
}

// seedMemberInA adds a member to household A directly, copying an existing
// member's password hash so the seeded account can sign in with the shared
// fixture password without reproducing the app's argon2id parameters here.
//
// Used for personas whose account state a test MUTATES irreversibly — enrolling
// MFA, for instance, changes every later login for that member — so the shared
// owner/adult/child personas stay usable by the rest of the suite.
function seedMemberInA({ displayName, email, role = 'adult', copyHashFrom }) {
  const existing = psql(
    `SELECT id FROM identity.member WHERE email = '${email}' LIMIT 1;`,
  ).trim();
  if (existing) return existing;

  return psql(`
    WITH household AS (
      SELECT id FROM identity.household WHERE name = 'Household A' LIMIT 1
    ), source AS (
      SELECT password_hash FROM identity.member WHERE email = '${copyHashFrom}' LIMIT 1
    )
    INSERT INTO identity.member
      (id, household_id, display_name, role, email, password_hash, active, created_at, updated_at)
    SELECT gen_random_uuid(), household.id, '${displayName}', '${role}',
           '${email}', source.password_hash, true, now(), now()
    FROM household, source
    RETURNING id;
  `).trim();
}

module.exports = { psql, seedHouseholdB, seedMemberInA };
