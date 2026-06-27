#!/bin/sh
# Usage: ./scripts/setup.sh
# Seeds your personal directories by copying the shipped examples:
#   data.example     -> data       (resume, context, target companies)
#   .context.example -> .context   (people/*.md read by the dashboard)
set -e

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# seed_dir SRC DEST: copy SRC to DEST unless DEST already exists.
seed_dir() {
  src="$1"
  dest="$2"
  if [ ! -d "$src" ]; then
    echo "Error: $(basename "$src") not found at $src"
    exit 1
  fi
  if [ -d "$dest" ]; then
    echo "$(basename "$dest")/ already exists, nothing copied."
    return 1
  fi
  cp -r "$src" "$dest"
  echo "Created $(basename "$dest")/ from $(basename "$src")"
  return 0
}

if seed_dir "$REPO_ROOT/data.example" "$REPO_ROOT/data"; then
  # Drop the example's stale SQLite so the app starts from an empty DB. Only
  # runs on a fresh copy, never against a data/ the user already populated.
  rm -f "$REPO_ROOT/data/jobs.db" "$REPO_ROOT/data/jobs.db-shm" "$REPO_ROOT/data/jobs.db-wal"
else
  echo "Delete data/ and re-run if you want a fresh start."
  DATA_SKIPPED=1
fi

# .context/ holds people/*.md (applicant.md, voice.md) the dashboard's Career
# detail tab reads. Without it that tab loads empty for a fresh install.
seed_dir "$REPO_ROOT/.context.example" "$REPO_ROOT/.context" || true

if [ -n "${DATA_SKIPPED:-}" ]; then
  exit 0
fi

# The installer (install.sh) sets SETUP_QUIET because it already brings the
# stack up and the in-browser wizard handles the Gemini key and resume. Only
# show the manual checklist when a developer runs this script directly.
if [ -z "${SETUP_QUIET:-}" ]; then
  echo ""
  echo "Next steps:"
  echo ""
  echo "  1. Edit your profile files in data/:"
  echo "       data/resume.md      your resume (plain text or markdown)"
  echo "       data/companies.json target search terms and company boards"
  echo "       data/companies.json target companies per ATS platform"
  echo ""
  echo "  2. Add your Gemini API key to .env:"
  echo "       GEMINI_API_KEY=your-key-here"
  echo ""
  echo "  3. Then bring it up:"
  echo "       docker compose up -d"
  echo ""
  echo "  Or: run 'docker compose up -d' and fill everything in via the setup wizard at localhost:3131"
  echo ""
fi
