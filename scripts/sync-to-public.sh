#!/usr/bin/env bash
#
# sync-to-public.sh — one-way mirror of the private repo to the public source-available
# repo (github.com/jakemercure28/sift-roles).
#
# Strategy: SOURCE-AVAILABLE core. The full tracked tree is published; only the paths in
# .publicignore (secrets, deploy/infra, observability, private docs) are stripped. Because
# nothing is removed from the Go compile graph, the public tree builds as-is.
#
# Safety model (each is a hard gate; any failure aborts before anything is pushed):
#   1. Stage = `git archive HEAD` — emits ONLY git-tracked files, so it is structurally
#      incapable of carrying .env / data/ / .context/ / node_modules (all gitignored).
#   2. .publicignore paths are deleted from the stage.
#   3. LICENSE is written deterministically as AGPL-3.0 (private LICENSE is MIT; never mirror it).
#   4. Secret scan over the stage (gitleaks if present, else value-shaped grep). Abort on hit.
#   5. `go build ./...` + `go vet ./...` — never publish a tree that does not compile.
#   6. Mirror into a clone of the public repo and push.
#
# Usage:
#   scripts/sync-to-public.sh --dry-run     # stage + all gates, no push (review first)
#   scripts/sync-to-public.sh               # ongoing sync: commit on top of public main
#   scripts/sync-to-public.sh --bootstrap   # one-time: replace public with a single clean commit
#
# Override the public remote with PUBLIC_REPO=<url>; default is the `public-target` remote.

set -euo pipefail

# --- locate the private repo root (where this script lives) ---
REPO_ROOT="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)"
cd "$REPO_ROOT"

DRY_RUN=0
BOOTSTRAP=0
for arg in "$@"; do
  case "$arg" in
    --dry-run)   DRY_RUN=1 ;;
    --bootstrap) BOOTSTRAP=1 ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done

PUBLIC_REPO="${PUBLIC_REPO:-$(git remote get-url public-target 2>/dev/null || true)}"
if [[ -z "$PUBLIC_REPO" ]]; then
  echo "ERROR: no public remote. Add one (git remote add public-target <url>) or set PUBLIC_REPO." >&2
  exit 1
fi

SRC_SHA="$(git rev-parse --short HEAD)"
STAGE="$(mktemp -d -t sift-public-stage.XXXXXX)"
PUBLIC_CLONE="$(mktemp -d -t sift-public-clone.XXXXXX)"
cleanup() { rm -rf "$STAGE" "$PUBLIC_CLONE"; }
trap cleanup EXIT

log() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }

# --- 1. stage tracked files only ---
log "Staging tracked files from HEAD ($SRC_SHA)"
git archive --format=tar HEAD | tar -x -C "$STAGE"

# --- 2. strip .publicignore paths ---
log "Applying .publicignore"
if [[ -f .publicignore ]]; then
  while IFS= read -r line; do
    line="${line%%#*}"; line="$(echo "$line" | xargs || true)"
    [[ -z "$line" ]] && continue
    if [[ -e "$STAGE/$line" ]]; then
      rm -rf "${STAGE:?}/$line"
      echo "  removed: $line"
    fi
  done < .publicignore
fi

# --- 3. deterministic AGPL-3.0 license ---
log "Writing AGPL-3.0 LICENSE"
cp "$REPO_ROOT/templates/LICENSE.public.agpl" "$STAGE/LICENSE"

# --- 4. secret-scan gate ---
log "Secret scan"
if [[ -e "$STAGE/.env" ]]; then
  echo "FATAL: .env present in stage — aborting." >&2; exit 1
fi

leak=0

# 4a. Precise leak check: does any REAL secret value from the local env files appear in the
#     stage? Zero false positives — we compare against the actual private values, redacted in
#     output. This is the primary gate; placeholder/test DSNs in tracked files do not match.
for sf in .env .env.local .env.selfhost.bak; do
  [[ -f "$REPO_ROOT/$sf" ]] || continue
  while IFS= read -r kv; do
    key="${kv%%=*}"; val="${kv#*=}"
    val="${val%\"}"; val="${val#\"}"; val="${val%\'}"; val="${val#\'}"
    # Only credential-bearing keys; skip plain config (TZ, DATABASE_TYPE, ports, booleans).
    case "$key" in
      *KEY|*TOKEN|*SECRET|*PASSWORD|*PASSWD|*CREDENTIAL|*_DSN|DATABASE_URL|RLS_DATABASE_URL|SUPABASE_*) ;;
      *) continue ;;
    esac
    [[ ${#val} -lt 8 ]] && continue
    case "$val" in *'<'*|*'your-'*|*'changeme'*|*'example'*|*'placeholder'*) continue;; esac
    if grep -rqFI -- "$val" "$STAGE" 2>/dev/null; then
      echo "FATAL: value of $key (from $sf) found in public stage" >&2
      leak=1
    fi
  done < <(grep -E '^[A-Za-z_][A-Za-z0-9_]*=.' "$REPO_ROOT/$sf" 2>/dev/null || true)
done

# 4b. High-signal patterns: credential shapes that have essentially no legitimate reason to live
#     in source (real API keys, JWTs, private keys). Deliberately NOT matching bare DSNs, which
#     appear as examples/tests in a source-available repo.
if command -v gitleaks >/dev/null 2>&1; then
  gitleaks detect --no-git --source "$STAGE" --redact || leak=1
else
  echo "  (gitleaks not found; using high-signal grep fallback)"
  PATTERNS='AIza[0-9A-Za-z_-]{30,}|hf_[0-9A-Za-z]{20,}|-----BEGIN[[:space:]][A-Z ]*PRIVATE KEY-----'
  if grep -rEnI "$PATTERNS" "$STAGE" 2>/dev/null; then
    echo "FATAL: high-signal secret pattern detected in stage" >&2; leak=1
  fi
fi

if [[ "$leak" == "1" ]]; then
  echo "FATAL: secret-scan gate failed — nothing pushed." >&2; exit 1
fi
echo "  clean"

# --- 5. build gate ---
log "Build gate (go build + go vet)"
( cd "$STAGE" && go build ./... && go vet ./... )
echo "  public tree compiles"

# --- summary ---
log "Staged public tree summary"
( cd "$STAGE" && find . -type f | sed 's|^\./||' | sort | head -50 )
echo "  ... $(cd "$STAGE" && find . -type f | wc -l | xargs) files total"

if [[ "$DRY_RUN" == "1" ]]; then
  log "DRY RUN — stage at $STAGE (not deleted on dry-run)"
  trap - EXIT; rm -rf "$PUBLIC_CLONE"
  echo "Inspect: $STAGE"
  exit 0
fi

# --- 6. mirror + push ---
if [[ "$BOOTSTRAP" == "1" ]]; then
  log "BOOTSTRAP — replacing public repo with a single clean commit"
  rm -rf "$PUBLIC_CLONE"; mkdir -p "$PUBLIC_CLONE"
  ( cd "$PUBLIC_CLONE"
    git init -q -b main
    rsync -a "$STAGE/" ./
    git add -A
    git commit -q -m "Sift: source-available core (from private $SRC_SHA)"
    git remote add origin "$PUBLIC_REPO"
    git push -f origin main )
  echo "  force-pushed single commit to $PUBLIC_REPO"
else
  log "SYNC — mirroring onto public main"
  git clone --depth 1 "$PUBLIC_REPO" "$PUBLIC_CLONE" 2>&1 | tail -1
  rsync -a --delete --exclude='.git/' "$STAGE/" "$PUBLIC_CLONE/"
  ( cd "$PUBLIC_CLONE"
    git add -A
    if git diff-index --quiet HEAD --; then
      echo "  no changes — public already up to date"
    else
      git commit -q -m "sync: upstream updates from core (private $SRC_SHA)"
      git push origin HEAD:main
      echo "  pushed update to $PUBLIC_REPO"
    fi )
fi

log "Done"
