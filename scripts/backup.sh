#!/bin/bash
set -euo pipefail

# Local snapshot of your job-search data.
# Usage: ./scripts/backup.sh [destination-dir]   (default: ./backups)
#
# Uses SQLite's native .backup so it's safe to run while the worker is up.
# Pair this with Time Machine / iCloud / File History for off-machine safety.

cd "$(dirname "$0")/.."

BACKUP_DIR="${1:-./backups}"
STAMP="$(date +%Y%m%d-%H%M%S)"

mkdir -p "$BACKUP_DIR"

echo "[backup] Snapshotting SQLite database..."
if [ -f "data/jobs.db" ]; then
  out="$BACKUP_DIR/jobs_${STAMP}.db"
  sqlite3 data/jobs.db ".backup '$out'"
  echo "  -> $out"
elif command -v docker >/dev/null 2>&1 && docker compose ps -q go-backend >/dev/null 2>&1; then
  out="$BACKUP_DIR/jobs_${STAMP}.db"
  # The Dockerized DB lives in the job_search_db named volume. go-backend is a
  # distroless image (no shell, no sqlite), so snapshot it from a throwaway alpine
  # attached to that volume — sqlite's .backup is safe against the live DB.
  vol="${COMPOSE_PROJECT_NAME:-$(basename "$(pwd)")}_job_search_db"
  docker run --rm \
    -v "${vol}:/db:ro" \
    -v "$(cd "$BACKUP_DIR" && pwd):/out" \
    alpine:3 sh -c "apk add --no-cache -q sqlite && sqlite3 /db/jobs.db \".backup '/out/jobs_${STAMP}.db'\""
  echo "  -> $out"
else
  echo "  -> skipped; no local data/jobs.db and dashboard container is not running"
fi

echo "[backup] Archiving data/..."
tar -czf "$BACKUP_DIR/data_${STAMP}.tar.gz" data/
echo "  -> $BACKUP_DIR/data_${STAMP}.tar.gz"

echo "[backup] Done. Snapshot saved to $BACKUP_DIR"
