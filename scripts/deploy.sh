#!/usr/bin/env bash
# Environment-aware pull deploy for the ThinkPad host.
#
# Runs on the box (invoked by the self-hosted GitHub Actions runner, or by hand
# over SSH). It fetches the target ref into that environment's checkout, rebuilds
# the compose stack, waits for every service with a healthcheck to report
# healthy, and rolls back to the previous commit if the new build never gets
# there.
#
# Usage:
#   deploy.sh <staging|prod> [ref]
#     staging -> dev branch  -> /srv/jsa/staging -> compose project jsa-staging
#     prod    -> main branch -> /srv/jsa/prod    -> compose project jsa-prod
#
# IMPORTANT: run this from a copy OUTSIDE the deploy dir (the runner copies it to
# $RUNNER_TEMP first). It git-resets the deploy dir, so executing it from inside
# that tree would rewrite the script mid-run.
#
# Each env's checkout carries its own gitignored .env / data/ / .context/, which
# is how prod and staging point at different Supabase projects and ports. Nothing
# secret lives in git.
set -euo pipefail

ENV_NAME="${1:?usage: deploy.sh <staging|prod> [ref]}"
REF_ARG="${2:-}"

case "${ENV_NAME}" in
  staging)
    DEPLOY_DIR="${STAGING_DIR:-/srv/jsa/staging}"
    export COMPOSE_PROJECT_NAME="jsa-staging"
    DEFAULT_BRANCH="dev"
    COMPOSE_FILES=(-f docker-compose.yml -f docker-compose.staging.yml)
    ;;
  prod)
    DEPLOY_DIR="${PROD_DIR:-/srv/jsa/prod}"
    export COMPOSE_PROJECT_NAME="jsa-prod"
    DEFAULT_BRANCH="main"
    COMPOSE_FILES=(-f docker-compose.yml)
    ;;
  *)
    echo "unknown environment: ${ENV_NAME} (want staging|prod)" >&2
    exit 2
    ;;
esac

REF="${REF_ARG:-${DEFAULT_BRANCH}}"
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-180}"
HEALTH_INTERVAL=5

log() { printf '\033[1m[deploy:%s]\033[0m %s\n' "${ENV_NAME}" "$*"; }
err() { printf '\033[31m[deploy:%s] %s\033[0m\n' "${ENV_NAME}" "$*" >&2; }

dc() { docker compose "${COMPOSE_FILES[@]}" "$@"; }

[[ -d "${DEPLOY_DIR}/.git" ]] || { err "no git checkout at ${DEPLOY_DIR} (run setup first)"; exit 2; }
cd "${DEPLOY_DIR}"

# --- Resolve target ref ------------------------------------------------------
PREV_SHA="$(git rev-parse HEAD)"
log "Fetching origin"
git fetch --prune origin
log "Checking out ${REF}"
git checkout --force "${REF}"
git reset --hard "origin/${REF}" 2>/dev/null || git reset --hard "${REF}"
TARGET_SHA="$(git rev-parse HEAD)"
log "Deploying ${TARGET_SHA} (was ${PREV_SHA}) in ${DEPLOY_DIR}"

# --- Health helpers ----------------------------------------------------------
service_ids() { dc ps --quiet; }

# 0 only when every container that defines a healthcheck is "healthy"; ones
# without a healthcheck must at least be "running".
all_healthy() {
  local ids cid st hl
  ids="$(service_ids)"
  [[ -z "${ids}" ]] && return 1
  while read -r cid; do
    [[ -z "${cid}" ]] && continue
    st="$(docker inspect -f '{{.State.Status}}' "${cid}" 2>/dev/null || echo missing)"
    hl="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "${cid}" 2>/dev/null || echo none)"
    if [[ "${hl}" == "none" ]]; then
      [[ "${st}" == "running" ]] || return 1
    else
      [[ "${hl}" == "healthy" ]] || return 1
    fi
  done <<< "${ids}"
  return 0
}

wait_for_health() {
  local waited=0
  while (( waited < HEALTH_TIMEOUT )); do
    if all_healthy; then
      log "All services healthy after ${waited}s"
      return 0
    fi
    sleep "${HEALTH_INTERVAL}"
    waited=$(( waited + HEALTH_INTERVAL ))
    log "  ...waiting on health (${waited}/${HEALTH_TIMEOUT}s)"
  done
  return 1
}

bring_up() {
  log "Building images"
  dc build
  log "Starting stack"
  dc up -d --remove-orphans
}

# The shared jsa-observability stack is NOT reconciled here any more. It used to
# ride the tail of a staging deploy, but Phase 6 gated this script to prod/main,
# which silently left it with no CD at all. It now has its own dev-tracking
# checkout and trigger: scripts/deploy-observability.sh.

# --- Deploy ------------------------------------------------------------------
bring_up

if wait_for_health; then
  log "Deploy of ${TARGET_SHA} succeeded"
  dc ps
  docker image prune -f >/dev/null 2>&1 || true
  exit 0
fi

# --- Rollback ----------------------------------------------------------------
err "New build never became healthy. Recent state + logs:"
dc ps || true
dc logs --no-color --tail=80 || true

if [[ "${PREV_SHA}" == "${TARGET_SHA}" ]]; then
  err "No previous commit to roll back to (already at ${PREV_SHA}). Stack left up for inspection."
  exit 1
fi

err "Rolling back to ${PREV_SHA}"
git reset --hard "${PREV_SHA}"
bring_up
if wait_for_health; then
  err "Rolled back to ${PREV_SHA} and recovered. Deploy of ${TARGET_SHA} FAILED."
  exit 1
fi
err "Rollback to ${PREV_SHA} ALSO failed to become healthy. Manual intervention required."
exit 2
