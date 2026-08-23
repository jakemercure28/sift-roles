#!/usr/bin/env bash
# Pull deploy for the shared jsa-observability stack (Prometheus + Grafana +
# Loki + exporters) on the ThinkPad host.
#
# Runs on the box (invoked by the self-hosted GitHub Actions runner from dev, or
# by hand over SSH). Fetches dev into a checkout that exists ONLY to serve this
# stack's bind-mounted config, then reconciles the compose project.
#
# Usage:
#   deploy-observability.sh [ref]        # ref defaults to dev
#
# Why its own checkout and its own script:
#
#   This stack used to be reconciled by the tail of a staging deploy, because
#   the staging checkout was the tree Prometheus bind-mounted. The Phase 6 k3s
#   cutover gated compose deploys to main-only, so the staging leg stopped
#   running and this stack silently lost its CD: its config froze at whatever
#   the staging tree last had, with nothing to ever update it.
#
#   It is not moved into the prod/main checkout instead because the stack is
#   iterated on constantly (dashboards, alert rules) and that would put every
#   tweak behind a manual main promotion. It is not left in the staging tree
#   because that tree is the Phase 6 rollback target: git-resetting it would
#   quietly rewrite the code you would be rolling back TO.
#
#   So: its own tree, its own trigger, dev-tracking. Retires at Phase 8, when
#   the k3s Prometheus takes over and this stack goes away entirely.
#
# IMPORTANT: run from a copy OUTSIDE the deploy dir (the runner copies it to
# $RUNNER_TEMP first). It git-resets that tree and must not rewrite itself
# mid-run.
set -euo pipefail

REF="${1:-dev}"
DEPLOY_DIR="${OBS_DIR:-/srv/jsa/observability}"
PROJECT="jsa-observability"
COMPOSE_FILE="docker-compose.observability.yml"
READY_TIMEOUT="${READY_TIMEOUT:-60}"

log() { printf '\033[1m[obs]\033[0m %s\n' "$*"; }
err() { printf '\033[31m[obs] %s\033[0m\n' "$*" >&2; }

[[ -d "${DEPLOY_DIR}/.git" ]] || {
  err "no git checkout at ${DEPLOY_DIR}"
  err "one-time setup: sudo git clone <deploy-key-remote> ${DEPLOY_DIR} (see docs/k8s-migration.md, Phase 7)"
  exit 2
}
cd "${DEPLOY_DIR}"

PREV_SHA="$(git rev-parse HEAD)"
log "Fetching origin"
git fetch --prune origin
log "Checking out ${REF}"
git checkout --force "${REF}"
git reset --hard "origin/${REF}" 2>/dev/null || git reset --hard "${REF}"
TARGET_SHA="$(git rev-parse HEAD)"
log "Reconciling ${TARGET_SHA} (was ${PREV_SHA}) in ${DEPLOY_DIR}"

# `up -d` is idempotent: it recreates a container only when its compose config
# actually changed, so running this on every dev push is a cheap no-op.
( cd observability && docker compose -p "${PROJECT}" -f "${COMPOSE_FILE}" up -d --remove-orphans )

# Rules and scrape config are bind-mounted files, so a content-only edit does
# NOT trigger a recreate; Prometheus has to be told to reload them.
#
# Use the lifecycle HTTP endpoint (--web.enable-lifecycle), NOT SIGHUP: a signal
# sent during a fresh container's WAL replay lands before Prometheus installs
# its handler, and the default disposition kills it (exit 2). After a recreate
# this reload is a harmless no-op, since startup already read the new config.
waited=0
while (( waited < READY_TIMEOUT )); do
  if docker exec "${PROJECT}-prometheus-1" wget -qO- 127.0.0.1:9090/-/ready >/dev/null 2>&1; then
    docker exec "${PROJECT}-prometheus-1" wget -qO- --post-data='' 127.0.0.1:9090/-/reload >/dev/null 2>&1 || true
    log "Reconciled (Prometheus ready + config reloaded)"
    exit 0
  fi
  sleep 3
  waited=$(( waited + 3 ))
done

err "Prometheus not ready ${waited}s after reconcile; config left as-is"
exit 1
