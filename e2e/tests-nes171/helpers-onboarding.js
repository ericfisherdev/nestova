// Private Nestova server + database for the tests that need a state the shared
// checklist database can never be in again: an UNCONFIGURED server (the
// first-run setup wizard) or a database with NO household (onboarding,
// brand-new-household dashboards).
//
// The shared slot database already holds household A — every other spec
// depends on it, and Nestova provisions a single household, so it cannot be
// emptied mid-run. Instead these tests build the server binary from this
// worktree and run a second instance on the suite's port + 100, against its
// own database inside the SAME Postgres container the suite already uses
// (NESTOVA_E2E_PG_CONTAINER). Nothing here touches the shared database or the
// shared server, so the files that use it run green right after the `fresh`
// project, in any order.
const { execFileSync, spawn } = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { tool } = require('../tests/tools');

const REPO_ROOT = path.resolve(__dirname, '..', '..');
const PG_CONTAINER = process.env.NESTOVA_E2E_PG_CONTAINER || 'nestova-test-db';
const PG_PORT = process.env.NESTOVA_E2E_PG_PORT || '5443';
const PG_USER = process.env.NESTOVA_E2E_PG_USER || 'nestova';
// reset-db.sh's convention: the e2e Postgres password equals the user name.
const PG_PASSWORD = PG_USER;

const SUITE_PORT = Number(new URL(process.env.NESTOVA_BASE_URL || 'http://localhost:8099').port);
const PRIVATE_PORT = SUITE_PORT + 100;
const PRIVATE_BASE_URL = `http://localhost:${PRIVATE_PORT}`;
const WORK_DIR = path.join(os.tmpdir(), `nestova-nes171-private-${PRIVATE_PORT}`);
const BIN_DIR = path.join(WORK_DIR, 'bin');
const PID_FILE = path.join(WORK_DIR, 'server.pid');

const READY_TIMEOUT_MS = 60_000;
const STOP_TIMEOUT_MS = 10_000;
const PORT_RELEASE_MS = 3_000;

let binariesBuilt = false;

// buildBinaries compiles cmd/server and cmd/migrate from this worktree once per
// worker process. Always rebuilt rather than cached across runs, so a stale
// binary can never test yesterday's code; Go's build cache keeps it to seconds.
function buildBinaries() {
  if (binariesBuilt) return;
  fs.mkdirSync(BIN_DIR, { recursive: true });
  execFileSync(tool('go'), ['build', '-o', `${BIN_DIR}/`, './cmd/server', './cmd/migrate'], {
    cwd: REPO_ROOT,
    stdio: 'pipe',
  });
  binariesBuilt = true;
}

// adminPsql runs sql against the container's maintenance database, for
// statements (CREATE/DROP DATABASE) that cannot run inside the target database.
function adminPsql(sql) {
  execFileSync(tool('docker'), ['exec', PG_CONTAINER, 'psql', '-U', PG_USER, '-d', 'postgres', '-v', 'ON_ERROR_STOP=1', '-q', '-c', sql]);
}

// privatePsql mirrors tests/db.js psql, pointed at a private database.
function privatePsql(database, sql) {
  return execFileSync(
    tool('docker'),
    ['exec', '-i', PG_CONTAINER, 'psql', '-U', PG_USER, '-d', database, '-v', 'ON_ERROR_STOP=1', '-q', '-At'],
    { input: `SET search_path TO nestova, identity, public;\n${sql}` },
  ).toString();
}

// dsnFor is the DSN the app itself would use: host-mapped port, and the
// search_path pin the boot guard requires (NSTR-118).
function dsnFor(database) {
  return `postgres://${PG_USER}:${PG_PASSWORD}@127.0.0.1:${PG_PORT}/${database}` +
    '?sslmode=disable&options=-csearch_path%3Dnestova%2Cpublic';
}

// recreateDatabase drops and recreates database, leaving it completely empty
// (no schema), which is what a genuinely fresh install looks like.
function recreateDatabase(database) {
  adminPsql(`DROP DATABASE IF EXISTS ${database} WITH (FORCE);`);
  adminPsql(`CREATE DATABASE ${database};`);
}

// migrateDatabase applies every migration, the same way reset-db.sh does.
function migrateDatabase(database) {
  buildBinaries();
  execFileSync(path.join(BIN_DIR, 'migrate'), ['up'], {
    env: { PATH: process.env.PATH, HOME: process.env.HOME, APP_ENV: 'dev', DATABASE_URL: dsnFor(database) },
    stdio: 'pipe',
  });
}

// PrivateServer runs one Nestova process on PRIVATE_PORT and captures its log,
// so a test can assert the server never answered with a 5xx or panicked.
class PrivateServer {
  constructor(env) {
    this.env = env;
    this.logPath = path.join(WORK_DIR, 'server.log');
    this.proc = null;
  }

  // start launches the binary and waits until readyPath answers 200.
  async start(readyPath = '/healthz') {
    buildBinaries();
    freePort();
    fs.mkdirSync(path.join(WORK_DIR, 'media'), { recursive: true });
    fs.rmSync(path.join(WORK_DIR, 'cache'), { recursive: true, force: true });
    fs.mkdirSync(path.join(WORK_DIR, 'cache'), { recursive: true });
    const log = fs.openSync(this.logPath, 'w');
    this.proc = spawn(path.join(BIN_DIR, 'server'), [], {
      env: {
        PATH: process.env.PATH,
        HOME: process.env.HOME,
        APP_ENV: 'dev',
        PORT: String(PRIVATE_PORT),
        MEDIA_ROOT: path.join(WORK_DIR, 'media'),
        CACHE_DIR: path.join(WORK_DIR, 'cache'),
        ...this.env,
      },
      detached: true,
      stdio: ['ignore', log, log],
    });
    fs.closeSync(log);
    fs.writeFileSync(PID_FILE, String(this.proc.pid));
    await this.waitFor(readyPath);
  }

  // waitFor polls path on the private server until it answers 200. Also used
  // after the setup wizard, which restarts the process in normal mode.
  async waitFor(readyPath, timeoutMs = READY_TIMEOUT_MS) {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      if (this.proc && this.proc.exitCode !== null) {
        throw new Error(`private server exited (${this.proc.exitCode}); log:\n${this.logTail()}`);
      }
      try {
        const res = await fetch(`${PRIVATE_BASE_URL}${readyPath}`, { redirect: 'manual' });
        if (res.status === 200) return;
      } catch {
        // Not listening yet.
      }
      await new Promise((resolve) => setTimeout(resolve, 250));
    }
    throw new Error(`private server never answered ${readyPath} with 200; log:\n${this.logTail()}`);
  }

  async stop() {
    if (!this.proc || this.proc.exitCode !== null) return;
    fs.rmSync(PID_FILE, { force: true });
    const exited = new Promise((resolve) => this.proc.once('exit', resolve));
    process.kill(-this.proc.pid, 'SIGTERM');
    const timedOut = await Promise.race([
      exited.then(() => false),
      new Promise((resolve) => setTimeout(() => resolve(true), STOP_TIMEOUT_MS)),
    ]);
    if (timedOut) process.kill(-this.proc.pid, 'SIGKILL');
  }

  log() {
    return fs.existsSync(this.logPath) ? fs.readFileSync(this.logPath, 'utf8') : '';
  }

  logTail(lines = 30) {
    return this.log().split('\n').slice(-lines).join('\n');
  }

  // serverErrors returns every logged 5xx response and panic. "Any 500 is a
  // failure", so the specs assert this is empty after each test.
  serverErrors() {
    return this.log().split('\n').filter((line) => /"status":5\d\d|panic/.test(line));
  }
}

// isHarnessServer reports whether pid is still the server binary this harness
// built, so a recycled pid is never signalled.
function isHarnessServer(pid) {
  try {
    // A binary replaced by a rebuild since the process started reads as
    // "<path> (deleted)"; start() rebuilds before freePort(), so that is the
    // usual state of this harness's own orphan.
    const DELETED = ' (deleted)';
    let exe = fs.readlinkSync(`/proc/${pid}/exe`);
    if (exe.endsWith(DELETED)) exe = exe.slice(0, -DELETED.length);
    return exe === path.join(BIN_DIR, 'server');
  } catch {
    return false;
  }
}

// freePort stops the server an earlier, aborted run of THIS harness left behind
// (tracked by pid file) and refuses to go on if anything else holds
// PRIVATE_PORT: killing a foreign process would be wrong, and waitFor could
// otherwise get a 200 from its /healthz and test the wrong server.
function freePort() {
  if (fs.existsSync(PID_FILE)) {
    const pid = Number(fs.readFileSync(PID_FILE, 'utf8'));
    if (Number.isInteger(pid) && pid > 1 && isHarnessServer(pid)) {
      try {
        process.kill(-pid, 'SIGKILL');
      } catch {
        // Already gone.
      }
    }
    fs.rmSync(PID_FILE, { force: true });
  }
  // A SIGKILLed server can take a moment to release its socket.
  const deadline = Date.now() + PORT_RELEASE_MS;
  while (portInUse()) {
    if (Date.now() >= deadline) {
      throw new Error(`port ${PRIVATE_PORT} is held by a process this harness did not start; free it or change NESTOVA_BASE_URL`);
    }
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 100);
  }
}

// portInUse is true when anything listens on PRIVATE_PORT (fuser exits 0).
function portInUse() {
  try {
    execFileSync(tool('fuser'), [`${PRIVATE_PORT}/tcp`], { stdio: 'ignore' });
    return true;
  } catch {
    return false;
  }
}

// setupModeServer is an unconfigured server: no DATABASE_URL and no state
// file, so it serves only the first-run wizard. NESTOVA_FORCE_SETUP is needed
// because APP_ENV=dev is otherwise exempt (bootstrap.NeedsSetup); main() clears
// it again once the wizard completes, so the restart boots normally.
function setupModeServer() {
  const stateFile = path.join(WORK_DIR, 'state.json');
  fs.rmSync(stateFile, { force: true });
  return {
    stateFile,
    server: new PrivateServer({ NESTOVA_FORCE_SETUP: '1', NESTOVA_STATE_FILE: stateFile }),
  };
}

// stateFileServer is a normal boot of an app the wizard already configured:
// only the persisted state file, no DATABASE_URL and no force flag.
function stateFileServer(stateFile) {
  return new PrivateServer({ NESTOVA_STATE_FILE: stateFile });
}

// configuredServer is a normal-mode server against database.
function configuredServer(database) {
  return new PrivateServer({ DATABASE_URL: dsnFor(database) });
}

// removeAllHouseholds returns a private database to the pre-onboarding state.
// Every household-scoped table cascades from identity.household.
function removeAllHouseholds(database) {
  privatePsql(database, 'DELETE FROM identity.household;');
}

// onboard submits the first-run form on the private server and waits for the
// dashboard, the way a new owner would.
async function onboard(page, { householdName, displayName, email, password }) {
  await page.goto(`${PRIVATE_BASE_URL}/onboarding`);
  await page.fill('input[name="household_name"]', householdName);
  await page.fill('input[name="display_name"]', displayName);
  await page.fill('input[name="email"]', email);
  await page.fill('input[name="password"]', password);
  await page.click('button:has-text("Create household")');
  await page.waitForURL((u) => new URL(u).pathname === '/', { timeout: 15_000 });
}

module.exports = {
  PRIVATE_BASE_URL,
  PG_PORT,
  PG_USER,
  PG_PASSWORD,
  privatePsql,
  recreateDatabase,
  migrateDatabase,
  setupModeServer,
  stateFileServer,
  configuredServer,
  removeAllHouseholds,
  onboard,
};
