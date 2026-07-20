# Testing

Two tiers: the default suite, which is hermetic and needs nothing, and the
database-gated suites, which need a real Postgres.

## The default suite

```sh
make test        # go test -race -cover ./...
```

No database, no network, no containers. Gated tests skip themselves when
`NESTOVA_TEST_DATABASE_URL` is unset, which is what keeps this run
dependency-free.

## The database-gated suites

Set `NESTOVA_TEST_DATABASE_URL` and the gated tests run instead of skipping:

```sh
docker run -d --rm --name nestova-test-db \
  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=nestova_test \
  -p 127.0.0.1:55432:5432 postgres:16-alpine

# docker run -d returns before Postgres accepts connections.
until docker exec nestova-test-db pg_isready -U postgres -d nestova_test >/dev/null 2>&1; do
  sleep 1
done

export NESTOVA_TEST_DATABASE_URL="postgres://postgres:test@localhost:55432/nestova_test?sslmode=disable"
make test-gated
```

`make test-gated` names the gated packages explicitly. `go test ./...` with
the variable set works too and runs everything; the explicit target exists
so a gated run is deliberate and its package list is reviewable.

### Prerequisites

- **A Postgres reachable at that DSN.** Version 16 or 17 (production runs
  17; both are exercised).
- **A database named `test` or ending in `_test`.** Enforced as a safety
  rail: the harness refuses to run otherwise, because it drops and recreates
  schemas. `nestova_test` is the convention.
- **The `CREATEDB` privilege on that role.** New requirement (NES-149) — the
  harness creates a database per package on demand. A superuser like the
  container's default `postgres` role already has it; a purpose-made role
  needs it granted:

  ```sql
  ALTER ROLE nestova_test CREATEDB;
  ```

  Without it, gated tests fail with a `create database` error naming this
  document.

### Isolation model

Every gated package gets **its own database**, derived from the configured
one by appending a package suffix — `nestova_test` becomes
`nestova_test_tasks`, `nestova_test_auth`, `nestova_test_media`, and so on.

That per-package database is what makes a parallel run safe. Before
NES-149, every package reset and migrated the single shared database; Go
runs different packages' test binaries concurrently, so one package's
`migrate.Reset` could drop the schema out from under another package's
in-flight test. The classic symptom was `goose_db_version` claiming
versions whose tables no longer existed. (`go test -p 1` does not fix it —
that serializes *builds*, not test binaries.)

Writing a gated test:

```go
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return dbtest.NewIsolatedPool(t, "tasks")
}
```

`dbtest.NewIsolatedPool` (`internal/platform/db/dbtest`) does the rest:
skips when the env var is unset, enforces the name safety rail, creates the
derived database if missing, resets and migrates it, and registers cleanup.

- The **suffix must be unique per package** and stable. Two packages sharing
  one would reintroduce exactly the race this removes.
- Need the connection string rather than a pool — a second pool in the same
  test, or a CLI invocation — use `dbtest.DSN(t, "<same-suffix>")`. Do not
  read `NESTOVA_TEST_DATABASE_URL` directly: that names the *base* database,
  not the package's, so the two would silently diverge.
- A package whose rows can block a down-migration passes a hook:
  `dbtest.NewIsolatedPool(t, "media", dbtest.WithPreReset(preResetSweep))`.
  `media/adapter` and `cmd/storage` use this to clear `s3`-stamped photo
  rows that migration 00032's rollback guard would otherwise refuse to drop.

Derived databases persist between runs; only their schemas are reset (on
both setup and cleanup), so repeat runs are fast. Drop them wholesale by
dropping the container, or:

```sql
-- inside psql, connected to the maintenance database. \gexec runs each
-- statement the SELECT generates; without it this only prints them.
SELECT format('DROP DATABASE %I;', datname)
  FROM pg_database
 WHERE datname LIKE 'nestova\_test\_%' ESCAPE '\'
\gexec
```

### The one exception

`internal/platform/db/migrate/migrate_test.go` does **not** use `dbtest`.
It tests the migration primitives (`Reset`/`Up`/`UpTo`) that `dbtest` is
built on, so importing it would be an import cycle — and layering those
tests over the helper they underpin would be backwards regardless. It
carries a small inline copy of the derivation logic instead, documented at
that helper.

`internal/platform/db/db_test.go` also reads the variable directly, but it
is not a schema-mutating test: it only opens a connection and pings it, so
it has nothing to isolate and cannot corrupt another package's fixture.

## S3-gated tests

A few media tests additionally need MinIO, gated on
`NESTOVA_TEST_S3_ENDPOINT`:

```sh
docker run -d --rm --name nestova-test-minio \
  -e MINIO_ROOT_USER=test -e MINIO_ROOT_PASSWORD=testtest123 \
  -p 127.0.0.1:59001:9000 minio/minio server /data

export NESTOVA_TEST_S3_ENDPOINT=http://127.0.0.1:59001
```

Each such test creates its own uniquely-named, disposable bucket.

## End-to-end (Playwright)

The `e2e/` suite drives a running server with a real browser and is not part
of `make test` or CI. It needs a server on `NESTOVA_BASE_URL` (default
`http://localhost:8099`) with an onboarded household, plus `NESTOVA_EMAIL`
and `NESTOVA_PASSWORD`:

```sh
cd e2e && npm ci && npx playwright install chromium
NESTOVA_EMAIL=... NESTOVA_PASSWORD=... npx playwright test
```

The suite shares one long-lived household and does not reset between tests,
so specs must tolerate pre-existing data (count-before/count-after rather
than absolute counts) and must not upload byte-identical files, which the
per-household content-hash dedup would reject as duplicates (NES-148).
