#!/bin/sh
# Usage: ./scripts/test-all.sh
# Runs the Go suite against BOTH storage engines so dialect/rewriter regressions
# can't slip through:
#   1. SQLite track  — plain `go test ./...` (the default backend).
#   2. Postgres track — spins up a throwaway postgres:16 container, exports
#      JSA_PG_DSN (the gate honored by internal/db/*_pg_test.go), and re-runs the
#      db tests against it.
# Requires Docker for the Postgres track; set SKIP_PG=1 to run SQLite only.
set -e

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

echo "==> SQLite track: go test ./..."
go test ./...

if [ "${SKIP_PG:-0}" = "1" ]; then
  echo "==> Postgres track skipped (SKIP_PG=1)"
  exit 0
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "==> Postgres track skipped (docker not found); set SKIP_PG=1 to silence" >&2
  exit 0
fi

PG_CONTAINER="jsa-pg-test-$$"
PG_PORT="${JSA_PG_PORT:-55432}"
PG_PASSWORD="spike"

cleanup() {
  docker rm -f "$PG_CONTAINER" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

echo "==> Postgres track: starting throwaway postgres:16 on :$PG_PORT"
docker run -d --rm \
  --name "$PG_CONTAINER" \
  -e POSTGRES_PASSWORD="$PG_PASSWORD" \
  -e POSTGRES_DB=jsa \
  -p "$PG_PORT:5432" \
  postgres:16 >/dev/null

echo "==> waiting for Postgres to accept connections"
i=0
until docker exec "$PG_CONTAINER" pg_isready -U postgres -d jsa >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -gt 60 ]; then
    echo "Postgres did not become ready in time" >&2
    exit 1
  fi
  sleep 1
done

export JSA_PG_DSN="postgres://postgres:$PG_PASSWORD@localhost:$PG_PORT/jsa?sslmode=disable"
echo "==> Postgres track: go test ./internal/db/... (JSA_PG_DSN set)"
go test -count=1 ./internal/db/...

echo "==> both tracks passed"
