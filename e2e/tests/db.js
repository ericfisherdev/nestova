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

module.exports = { psql };
