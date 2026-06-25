# Spike: PocketBase as a database backend (NES-82)

Status: **Design / recommendation** — no production code in this spike.
Epic: NES-81 (PocketBase database backend support).

## 1. Goal

Decide whether and how Nestova can run on **PocketBase**, alongside the
already-supported Postgres family (local/remote Postgres and Supabase
cloud/local — both are Postgres). The driver is the local-first LAN-appliance
target: a single self-contained, fully-offline backend on a Raspberry Pi without
operating a separate Postgres server.

## 2. Where we are today

The persistence layer is exclusively Postgres and reaches into every bounded
context:

- A `pgxpool` connection pool; repositories depend on the `db.TX` interface
  (satisfied by `*pgxpool.Pool` and `pgx.Tx`). ~37 files import `pgx`.
- Schema via **goose** migrations on the Postgres dialect (17 migrations),
  using Postgres-only features: `jsonb`, `uuid`, `text[]`, `numeric`,
  `timestamptz`, `ON CONFLICT`, `gen_random_uuid()`.
- Two concurrency seams use **`pg_advisory_xact_lock`**: the first-run
  onboarding provisioner (`cmd/server/provisioning.go`) and the gamification
  ledger (`internal/tasks/adapter/gamification_postgres.go`).
- Sessions via `scs` backed by **`pgxstore`**.
- `DB_PROVIDER` selects `postgres | supabase`; the config comment is explicit:
  *"Both are Postgres; the provider only changes connectivity … never the schema
  or queries."*

The architecture is hexagonal: each context defines repository **ports** in its
`domain` package, and the composition root wires concrete `pgx` adapters. That
port boundary is what makes an alternate backend conceivable — but the
migrations, advisory locks, session store, and Postgres-specific SQL are all
hard-bound to Postgres.

## 3. What PocketBase actually is

PocketBase (context7: `/pocketbase/pocketbase`) is a **Go application framework**,
not a database server you connect to over a wire protocol:

- `pocketbase.New()` owns the HTTP server, the embedded **SQLite** database, an
  admin dashboard, auth, and realtime subscriptions.
- Data access is via `app.Dao()` / records / **collections** and the `dbx` query
  builder over SQLite. Transactions: `app.RunInTransaction(func(txApp core.App) …)`.
- Schema is defined as **collections** (typed fields + indexes + access rules),
  versioned through Go migrations registered with `core.AppMigrations.Register(up, down)`.
- There is **no first-class Go client SDK** for an external PocketBase server —
  the official SDKs are JavaScript and Dart. External Go usage means raw REST.

The key structural fact: PocketBase wants to **own the runtime** (HTTP + DB +
auth + UI). Nestova already provides its own `net/http` server
(`internal/platform/httpserver`), `templ` UI, and `scs` session/auth
(`internal/auth/adapter/session.go`). PocketBase's HTTP, auth, and admin-UI
layers therefore **overlap** with what Nestova already has; its realtime
subscriptions are a capability Nestova lacks, but this app does not need them.

## 4. Approaches evaluated

### A. PocketBase as an embedded Go framework, DB layer only

Import PocketBase and use only its DB layer (`app.Dao()` / `dbx` over the embedded
SQLite), keeping Nestova's own HTTP/templ/scs stack.

- Pros: single binary; reuses PocketBase's SQLite bootstrap.
- Cons: PocketBase expects to own `app.Start()`/serve; running it **headless**
  (DB only, no server, no admin) is off the supported path and fragile across
  upgrades. We would pull in the entire PocketBase dependency tree for just its
  SQLite handle. We would still rewrite every repository against `dbx`/collections.
- Verdict: high coupling to an unsupported usage for little benefit over targeting
  SQLite directly.

### B. External PocketBase server + REST

Nestova talks to a separate PocketBase over HTTP.

- Pros: clean process separation; PocketBase admin UI available.
- Cons: no first-class Go SDK; **no cross-record transactions and no advisory
  locks over REST**, so the transactional ports (onboarding provisioner,
  gamification ledger) cannot be satisfied without unsafe workarounds; an extra
  network hop and a second service to run on the appliance — the opposite of the
  "single offline binary" goal.
- Verdict: **reject** — incompatible with the transactional repository ports and
  the deployment goal.

### C. Direct SQLite (no PocketBase)

Use a pure-Go SQLite driver (`modernc.org/sqlite`) and implement the existing
repository ports against SQLite directly.

- Pros: single embedded file, fully offline, zero extra services — the cleanest
  fit for the appliance. SQLite supports transactions and `ON CONFLICT`. Pure-Go
  driver means no cgo, easy cross-compile for the Pi. Reuses the existing
  hexagonal ports unchanged.
- Cons: it is **not "PocketBase"** — no admin UI / auth / realtime (all of which
  Nestova already provides itself). Still requires a SQLite repository set,
  schema port, and concurrency rework.
- Verdict: **strong fit** for the stated goal.

## 5. Hard problems (apply to A and C; B is rejected)

| Concern | Postgres today | SQLite / PocketBase plan |
| --- | --- | --- |
| Transactions / `db.TX` | `pgxpool` + `pgx.Tx` | `database/sql` `*sql.DB`/`*sql.Tx` behind the same `db.TX`-shaped seam; one shared abstraction, two adapters. |
| Advisory locks | `pg_advisory_xact_lock` (onboarding, gamification) | SQLite is single-writer; the correctness boundary is a `BEGIN IMMEDIATE` transaction **plus the existing unique constraints/indexes** (an in-process mutex is not sufficient — it only serializes threads in one process, not other connections). The onboarding first-household guard and the gamification ledger's uniqueness index carry over and do the enforcing. |
| Migrations | goose (postgres) | goose has a **sqlite3** dialect; maintain a parallel SQLite migration set (or a translation), porting the DDL. |
| Types / SQL | `jsonb`, `text[]`, `numeric`, `timestamptz`, `gen_random_uuid()` | `json`/`text`; `json`; **`numeric` → `TEXT` or scaled integers** (SQLite `REAL` is approximate — do not use it for exact-decimal/money fields); ISO-8601 `text`; app-generated UUIDv7. `ON CONFLICT` is supported by SQLite as-is. |
| Sessions | `scs` `pgxstore` | `scs` `sqlite3store` is the default (it persists sessions across restarts, matching the offline-appliance goal). `memstore` only for a deliberately ephemeral kiosk mode — it drops all auth state on restart. |
| Setup wizard | `postgres://` DSN | a new "embedded/SQLite" mode: a data-dir/file path instead of a DSN. |

## 6. Recommendation (go/no-go)

**Recommend GO on an embedded backend, implemented as direct SQLite (Approach C);
effectively NO-GO on PocketBase-proper.**

Rationale:

1. The actual requirement is *"a self-contained, offline backend for the
   appliance."* SQLite delivers that with the least architectural disruption,
   reusing the existing ports and keeping transactions + `ON CONFLICT`. It is not
   equivalent to Postgres — type affinity, no native `jsonb`, and single-writer
   concurrency differ — but those gaps are bounded and handled by a dedicated
   SQLite migration set and per-backend tests (see §5).
2. PocketBase's value is its BaaS bundle (admin UI, auth, realtime). Nestova
   already provides its own auth and UI, and this app does not use realtime, so
   that bundle adds little here. Adopting PocketBase means either inverting
   Nestova to run inside PocketBase's runtime (Approach A, fragile) or dropping
   transactions (Approach B, rejected), for features we do not need.
3. If the PocketBase **admin UI** is later wanted, that is a separate,
   **unverified** idea rather than an assumed follow-up: PocketBase expects to own
   its own `pb_data`, and pointing a second process at Nestova's SQLite file is
   risky (SQLite is single-writer, so concurrent writers contend on the file
   lock). It would need its own evaluation and is explicitly out of scope here.

So: deliver SQLite as the second supported backend under NES-81, and keep
"PocketBase" as an explicitly de-scoped option (documented, with this rationale).

## 7. Effort & phased breakdown for NES-81

Rough order-of-magnitude: **Large** (≈ a full sprint of focused work), dominated
by the repository rewrites. Proposed phases (each a task under NES-81):

1. **Backend selection + connection seam** — an engine selector: either extend
   the existing `DB_PROVIDER` (today `postgres|supabase`) to add `sqlite`, or
   introduce a separate `DB_BACKEND` engine axis with `DB_PROVIDER` nested under
   Postgres — decide and document the config shape (and any migration/alias).
   Plus a `database/sql` SQLite connection and a `db.TX` adapter, with
   composition-root wiring to choose the adapter set. (M)
2. **SQLite migration set** — goose sqlite3 dialect; port the 17 migrations,
   translating Postgres-only DDL; parity tests vs the Postgres schema. (L)
3. **Repository adapters per context** — auth, household, tasks, tracking, meals,
   subscriptions, calendar, media implemented against SQLite. The bulk. (XL —
   split per context)
4. **Concurrency seams** — replace the two `pg_advisory_xact_lock` sites with
   SQLite-safe serialization. (S)
5. **Session store** — `scs` `sqlite3store`. (S)
6. **Setup wizard** — an embedded/SQLite configuration mode (data-dir path). (M)
7. **CI** — run the unit + e2e suites against the SQLite backend in CI. (M)

The Postgres and Supabase paths stay unchanged throughout.

## 8. References

- PocketBase as a Go framework, `app.Dao()`/`dbx`, `RunInTransaction`, collection
  migrations — context7 `/pocketbase/pocketbase`.
- Current coupling: `internal/platform/db`, `internal/platform/db/migrate`,
  `internal/auth/adapter/session.go`, `cmd/server/provisioning.go`,
  `internal/tasks/adapter/gamification_postgres.go`, `internal/platform/config`.
