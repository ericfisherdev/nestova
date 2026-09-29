#!/usr/bin/env bash
# Drops and recreates the NES-171 e2e database, then re-applies migrations.
# The checklist run starts from an empty database on purpose: §1 tests the
# first-run path, and a stale household would make them unrunnable.
set -euo pipefail

CONTAINER="${NESTOVA_E2E_PG_CONTAINER:-nestova-test-db}"
DB="${NESTOVA_E2E_PG_DB:-nestova_test}"
USER="${NESTOVA_E2E_PG_USER:-nestova}"
PORT="${NESTOVA_E2E_PG_PORT:-5443}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

docker exec "$CONTAINER" psql -U "$USER" -d postgres -c "DROP DATABASE IF EXISTS $DB WITH (FORCE);" >/dev/null
docker exec "$CONTAINER" psql -U "$USER" -d postgres -c "CREATE DATABASE $DB;" >/dev/null

export DATABASE_URL="postgres://${USER}:${USER}@127.0.0.1:${PORT}/${DB}?sslmode=disable&options=-csearch_path%3Dnestova%2Cpublic"
export APP_ENV=dev
(cd "$REPO_ROOT" && go run ./cmd/migrate up >/dev/null 2>&1)

echo "reset: $DB recreated and migrated"
