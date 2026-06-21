# nestova

Nestova — family household management app (Go/Postgres/HTMX/Alpine/Tailwind/GSAP)

## Development

### Prerequisites

- Go (see the `go` directive in [`go.mod`](go.mod))
- [golangci-lint](https://golangci-lint.run) **v2.11.4** (see [Linting](#linting-golangci-lint))

Most developer tooling (templ, etc.) is pinned in `go.mod` via Go tool
directives, so no global install is needed — invoke it with `go tool <name>`.
golangci-lint is the exception: its maintainers advise against `go install`, so
it is installed as a pinned binary instead.

### Common tasks

```sh
make build      # compile the server into ./bin
make run        # run the server from source (serves /healthz on :8080)
make generate   # regenerate Go from .templ files
make fmt        # format templ and Go sources
make test       # run tests with the race detector + coverage profile
make cover      # print a per-function coverage summary
make hooks      # install the Lefthook Git hooks
make help       # list all targets
```

### Configuration

Configuration is read **only from environment variables** (so secrets are never
committed) and validated at startup by
[`internal/platform/config`](internal/platform/config/config.go). Startup
**fails fast** with a single error listing every missing or invalid value.

For local development, copy [`.env.example`](.env.example) to `.env` (gitignored)
and adjust as needed — it is loaded automatically when `APP_ENV=dev`. Real
environment variables always take precedence over `.env`.

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `APP_ENV` | no | `dev` | Deployment environment: `dev`, `test`, or `prod`. |
| `PORT` | no | `8080` | HTTP listen port (a leading colon is tolerated). |
| `DATABASE_URL` | no in dev | docker-compose DSN | Postgres connection string. Override in prod. |
| `MIGRATE_DATABASE_URL` | no | `DATABASE_URL` | Separate DSN for the migration tool; point at a session/direct connection for Supabase (see [Database migrations](#database-migrations)). |
| `DB_MAX_CONNS` | no | `0` | Connection pool cap; `0` lets the pool choose (Supabase defaults to `10`). |
| `DB_CONNECT_TIMEOUT` | no | `5s` | Bounds the startup connectivity check (Go duration). |
| `DB_PROVIDER` | no | `postgres` | Database backend: `postgres` or `supabase` (see [Using Supabase](#using-supabase)). |
| `DB_POOL_MODE` | no | `session` | Supabase pooler endpoint the DSN targets: `session` or `transaction`. Consulted only for `supabase`. |
| `DB_SSL_ROOT_CERT` | no | — | Path to a CA bundle; enables `sslmode=verify-full`. |
| `SESSION_SECRET` | yes in prod | dev-only default | Signs session cookies; ≥ 32 bytes. The dev default is rejected in prod. |
| `SESSION_LIFETIME` | no | `12h` | Maximum session duration (Go duration). |
| `GOOGLE_CLIENT_ID` | yes in prod | — | Google OAuth client ID (Google Calendar sync). |
| `GOOGLE_CLIENT_SECRET` | yes in prod | — | Google OAuth client secret. |
| `GOOGLE_REDIRECT_URL` | yes in prod | — | Google OAuth redirect URL. |

In `prod`, cookies are automatically marked `Secure`, and `DATABASE_URL`,
`SESSION_SECRET`, and the Google OAuth credentials must be supplied explicitly
(the dev defaults are rejected).

> **Production secrets:** environment variables are appropriate for development
> and small deployments. For production at scale, source secrets from a
> dedicated manager (e.g. HashiCorp Vault, AWS/GCP Secrets Manager, or
> Kubernetes Secrets) and inject them into the environment, rather than storing
> them in plaintext `.env` files.

### Database (local Postgres)

The app connects to Postgres on boot via a pgx connection pool
([`internal/platform/db`](internal/platform/db/db.go)) and **fails fast** if the
database is unreachable. Start the bundled local Postgres before running the
server:

```sh
docker compose up -d        # starts postgres:17-alpine on :5432
docker compose down         # stop it (add -v to drop the data volume)
```

The default `DATABASE_URL` matches this service, so no extra configuration is
needed for local development. Pool sizing is `DB_MAX_CONNS` (0 lets pgx choose
its default, itself the CPU count with a floor of 4); the startup connectivity
check is bounded by `DB_CONNECT_TIMEOUT` (see [Configuration](#configuration)).

The database-backed tests are skipped unless `NESTOVA_TEST_DATABASE_URL` points
at a reachable test database, so `make test` stays hermetic by default.

### Using Supabase

[Supabase](https://supabase.com) is managed Postgres, so Nestova runs on it with
**no schema or query changes** — only connectivity differs. The bundled
docker-compose Postgres remains the default; Supabase is opt-in via configuration.

> **Scope:** this covers the **Postgres database** only. Supabase Auth, Storage,
> Realtime, and PostgREST are not used — Nestova brings its own auth/session stack
> (NES-23).

**1. Get the connection strings.** In the Supabase dashboard under *Project
Settings → Database*, three connection strings are offered:

| Connection | Port | Use it for |
| --- | --- | --- |
| Direct connection | 5432 | Migrations, and a long-running server where a direct route is available. |
| Session pooler | 5432 | A long-running server (Nestova): one backend connection per client, so cached prepared statements are safe. |
| Transaction pooler | 6543 | Short-lived/serverless clients. Multiplexes per transaction — set `DB_POOL_MODE=transaction` so Nestova uses the pooler-safe exec mode. |

Nestova is a long-running server, so prefer the **session pooler** or **direct
connection** for `DATABASE_URL`. If you must use the **transaction pooler** (port
6543), set `DB_POOL_MODE=transaction`; Nestova then disables cached server-side
prepared statements, which Supabase's Supavisor pooler cannot keep across
multiplexed transactions.

**2. Require TLS.** Supabase accepts only TLS connections. Keep `sslmode=require`
in the DSN — Supabase rejects `sslmode=disable`, and so does Nestova when
`DB_PROVIDER=supabase`. For full certificate verification, download Supabase's CA
bundle and set `DB_SSL_ROOT_CERT=/path/to/ca.crt`; Nestova then upgrades the
connection to `sslmode=verify-full`.

**3. Configure Nestova.**

```sh
DB_PROVIDER=supabase
# Set transaction only when DATABASE_URL targets the :6543 pooler; otherwise session.
DB_POOL_MODE=transaction
DATABASE_URL=postgres://postgres.<ref>:<password>@<region>.pooler.supabase.com:6543/postgres?sslmode=require
# DDL and goose version bookkeeping need a session connection, so point the
# migration tool at the direct/session connection (port 5432):
MIGRATE_DATABASE_URL=postgres://postgres.<ref>:<password>@<region>.pooler.supabase.com:5432/postgres?sslmode=require
# Optional: verify the server certificate against Supabase's CA (enables verify-full):
# DB_SSL_ROOT_CERT=/path/to/supabase-ca.crt
```

When `DB_PROVIDER=supabase` and `DB_MAX_CONNS` is unset, Nestova defaults to a
modest pool cap (`10`) appropriate behind the shared pooler; adjust it to fit your
Supabase plan's connection limits.

**4. Migrate and run.**

```sh
make migrate-up    # applies migrations via MIGRATE_DATABASE_URL (direct/session)
make run
```

### Front-end assets

The UI is styled with [Tailwind CSS v4](https://tailwindcss.com) and made
interactive with vendored, pinned [HTMX](https://htmx.org) and
[Alpine.js](https://alpinejs.dev). There is **no Node toolchain**: the Tailwind
**standalone CLI** (pinned in the `Makefile`, checksum-verified) builds the CSS.

```sh
make assets      # download the pinned Tailwind CLI if missing, then build app.css
```

- [`web/static/css/input.css`](web/static/css/input.css) defines the **A · Hearth**
  design tokens in a `@theme static` block (sand/sage palette, the 5-set member
  color system, warm-ink text, radii, soft shadows) and `@source`s the `.templ`
  files. The build output, [`web/static/css/app.css`](web/static/css/app.css), is
  committed so plain `go build` works.
- HTMX and Alpine are vendored under `web/static/js/` (pinned versions); fonts
  (Hanken Grotesk, Space Mono) are **self-hosted** under `web/static/fonts/` so
  the entryway appliance works offline.
- All assets are embedded into the binary ([`web/web.go`](web/web.go)) and served
  under `/static/`. `make build` runs `make assets` first so the embedded CSS is
  always fresh; CI rebuilds it to prove reproducibility.

The Tailwind CLI version and per-platform checksums are pinned in the `Makefile`
(`TAILWIND_VERSION` / `TAILWIND_SHA256`); the build auto-detects and
checksum-verifies the correct binary for Linux and macOS (x64/arm64). Tailwind's
CSS output is platform-independent, so the committed `app.css` matches CI's
Linux rebuild regardless of where it was built. On an unsupported platform,
download the matching release asset manually into `tools/bin/tailwindcss`.

### Database migrations

Schema migrations are SQL files embedded into the binary
([`internal/platform/db/migrate/migrations`](internal/platform/db/migrate/migrations))
and applied with [goose](https://github.com/pressly/goose) over the pgx stdlib
driver. Migrations run **explicitly** (never automatically on server boot).

Ensure `DATABASE_URL` is configured before running migrations (see
[Configuration](#configuration)).

```sh
make migrate-up                     # apply all pending migrations
make migrate-down                   # roll back the most recent migration
make migrate-status                 # show applied/pending migrations
make migrate-reset                  # roll back every migration
make migrate-create name=add_foo    # scaffold internal/.../migrations/NNNNN_add_foo.sql
```

`migrate-create` writes to the source tree, so run it from the repo root.
Migrations are numbered sequentially (`NNNNN_name.sql`) with `-- +goose Up` /
`-- +goose Down` sections. The baseline (`00001_baseline.sql`) creates the
`household`, `member`, and `notification` tables.

Migrations target `DATABASE_URL` by default. Set **`MIGRATE_DATABASE_URL`** to run
them against a different connection than the app server uses — required for
Supabase, where DDL and goose's version bookkeeping need a **session** connection
(direct connection or session pooler, port 5432) while the app server may point at
the transaction pooler (port 6543). Point `MIGRATE_DATABASE_URL` at the
direct/session connection. If migrations must run through the transaction pooler
(no separate `MIGRATE_DATABASE_URL` and `DB_PROVIDER=supabase` with
`DB_POOL_MODE=transaction`), the tool automatically falls back to the simple query
protocol so goose's bookkeeping does not rely on named prepared statements.

> **Run migrations serially.** Sequential numbering assumes migrations are
> created and applied one at a time. Create new migrations serially (`create`
> refuses to overwrite an existing file). In a multi-instance deployment, apply
> migrations from a single coordinated job rather than from each instance, so
> two processes never migrate concurrently.
>
> **Rollback caution.** `migrate-down` / `migrate-reset` run the `-- +goose Down`
> SQL, which can drop tables and destroy data. Test the up/down round-trip in
> development first (`migrate_test.go` exercises it against a test database), and
> take a backup before rolling back in production.

### Git hooks (Lefthook)

[Lefthook](https://lefthook.dev) is pinned as a Go tool directive in `go.mod`.
Enable the hooks once per clone:

```sh
make hooks            # go tool lefthook install
make hooks-uninstall  # remove them
```

The hooks delegate to the same `make` targets as CI, so local and CI checks
never diverge ([`lefthook.yml`](lefthook.yml)):

- **pre-commit** (piped, fails fast): format staged sources, verify generated
  `*_templ.go` is in sync, `make lint`, and `go test ./...`.
- **commit-msg**: enforce [Conventional Commits](https://www.conventionalcommits.org)
  (see [CONTRIBUTING.md](CONTRIBUTING.md#commit-messages)).
- **pre-push**: the full race-enabled `make test` plus `make lint`.

### Testing

See [docs/testing.md](docs/testing.md) for the project's test conventions
(table-driven, black-box, environment isolation) and coverage workflow.
`make test` writes a coverage profile to `coverage.out`; `make cover` renders a
per-function summary.

### Templates (templ)

The front end uses [templ](https://templ.guide) (`github.com/a-h/templ`), which
compiles type-safe `.templ` components to Go.

- The CLI is pinned as a Go tool directive; run it with `go tool templ` (e.g.
  `go tool templ generate`, `go tool templ fmt .`).
- **Generated files are committed.** Each `.templ` file has a sibling
  `*_templ.go` produced by `templ generate`; these are checked in so
  `go build ./...` works without templ installed and CI stays simple. They
  carry the `// Code generated by templ - DO NOT EDIT.` header and must never
  be edited by hand.
- After editing a `.templ` file, run `make generate` and commit the updated
  `*_templ.go` alongside it. CI verifies generated output is in sync.

### Notifications (NES-24)

The `internal/notify` bounded context implements a durable notification outbox.

**Package layout:**

| Package | Role |
| --- | --- |
| `internal/notify/domain` | `Notification` aggregate, `NotificationID`, `Channel`/`Status` enums, `Outbox`/`Sender` port interfaces, sentinel errors |
| `internal/notify/adapter` | `OutboxRepository` (pgx-backed `Outbox`), `InAppSender` (in-app `Sender`) |
| `internal/notify/app` | `Dispatcher` — polls the outbox and delivers via channel-specific senders |

**Outbox lifecycle:**

1. A producer calls `Outbox.Enqueue`, writing a `status='pending'` row.
2. `Dispatcher.RunOnce` calls `Outbox.ClaimDue(ctx, limit)`, which atomically
   selects due pending rows with `FOR UPDATE SKIP LOCKED` and transitions them
   to `status='sent'` (leaving `sent_at` NULL) in a single CTE+UPDATE.
   Concurrent dispatchers never claim the same row.
3. The dispatcher invokes the appropriate `Sender`. On success it calls
   `MarkSent` (which stamps `sent_at`). On failure it calls `MarkFailed`.

**Delivery semantics:** the optimistic-claim pattern (mark `sent` before the
send) leaves two gaps, both acceptable for the skeleton:
1. A crash after `ClaimDue` but before the send leaves a row `(status='sent',
   sent_at IS NULL)` that was never delivered — message loss (at-most-once).
2. A crash after a successful send but before `MarkSent` leaves a delivered row
   that a recovery sweep might re-dispatch — duplicate delivery (at-least-once).

The `(status='sent', sent_at IS NULL)` shape is deliberately detectable so a
future recovery sweep can close both gaps.

**Dispatcher configuration** (wired in `cmd/server/main.go`):
- Batch size: 50 notifications per poll cycle.
- Poll interval: 30 seconds.
- The dispatcher goroutine uses the same signal-cancelled context as the HTTP
  server, so it stops cleanly on `SIGINT`/`SIGTERM`.

### Linting (golangci-lint)

Static analysis is configured in [`.golangci.yml`](.golangci.yml) (schema v2).

- **Version is pinned** to `v2.11.4` (the `GOLANGCI_LINT_VERSION` variable in
  the `Makefile` is the single source of truth; CI installs this exact version
  via the official action). Install the matching release locally from the
  [install guide](https://golangci-lint.run/docs/welcome/install/) — do **not**
  use `go install`.
- `make lint` runs `golangci-lint run`; `make fmt` applies the configured
  formatters (gofumpt + goimports) via `golangci-lint fmt`.
- Generated `*_templ.go` files are excluded from linting and formatting via
  the `exclusions.generated: strict` setting under both `linters` and
  `formatters` in `.golangci.yml`.

## License

Nestova is licensed under the **GNU Affero General Public License v3.0**
(AGPL-3.0). See [`LICENSE`](LICENSE) for the full text.

In short: you are free to use, modify, and distribute this software, but if you
run a modified version as a network service, you must make the corresponding
source code available to its users.

Copyright (C) 2026 Eric Fisher
