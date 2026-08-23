#!/usr/bin/env bash
# Registry retention: keep the newest KEEP tags per repository, delete the
# rest via the registry API, then run the offline garbage collector to
# reclaim blob space. Tags are git SHAs (not sortable by name), so recency
# comes from the tag directory mtime in the registry's filesystem layout.
#
# Run weekly from root cron on the ThinkPad (see docs/k8s-migration.md).
# Safe to prune aggressively: every image is rebuildable from git.
set -euo pipefail

REGISTRY_URL="${REGISTRY_URL:-http://127.0.0.1:5000}"
REGISTRY_DATA="${REGISTRY_DATA:-/srv/jsa/registry}"
CONTAINER="${CONTAINER:-jsa-registry-registry-1}"
KEEP="${KEEP:-10}"

repos_dir="$REGISTRY_DATA/docker/registry/v2/repositories"
[ -d "$repos_dir" ] || { echo "no repositories yet"; exit 0; }

resolve_digest() {
  # Prints the manifest digest a tag currently points at, or nothing on failure.
  curl -fsS -o /dev/null -w '%header{docker-content-digest}' \
    -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
    -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
    "$REGISTRY_URL/v2/$1/manifests/$2" 2>/dev/null
}

deleted=0
# Find every repo (dirs that contain _manifests), relative to repos_dir.
while IFS= read -r manifests_dir; do
  repo="${manifests_dir#"$repos_dir"/}"
  repo="${repo%/_manifests}"
  tags_dir="$manifests_dir/tags"
  [ -d "$tags_dir" ] || continue

  # The registry API can only delete a manifest by digest, and a digest is
  # shared by every tag that resolves to it. Reproducible builds make this
  # routine: when a component's sources do not change between commits, each
  # commit tag points at one identical image. Deleting an old tag's digest
  # therefore unlinks the newest tags too, which is how a live prod tag can
  # 404 straight after a retention run.
  #
  # So resolve the newest KEEP tags first and treat their digests as pinned.
  declare -A keep_digests=()
  while IFS= read -r tag; do
    [ -n "$tag" ] || continue
    d=$(resolve_digest "$repo" "$tag") || continue
    [ -n "$d" ] && keep_digests["$d"]=1
  done < <(ls -1t "$tags_dir" | head -n "$KEEP")

  # Now prune the older tags, skipping any digest a kept tag still needs and
  # any digest already deleted in this pass.
  declare -A seen=()
  while IFS= read -r tag; do
    [ -n "$tag" ] || continue
    digest=$(resolve_digest "$repo" "$tag") || continue
    [ -n "$digest" ] || continue
    if [ -n "${keep_digests[$digest]:-}" ]; then
      echo "keeping $repo:$tag ($digest): still referenced by a retained tag"
      continue
    fi
    [ -n "${seen[$digest]:-}" ] && continue
    seen["$digest"]=1
    echo "deleting $repo:$tag ($digest)"
    curl -fsS -X DELETE "$REGISTRY_URL/v2/$repo/manifests/$digest" || true
    deleted=$((deleted + 1))
  done < <(ls -1t "$tags_dir" | tail -n "+$((KEEP + 1))")
  unset keep_digests seen
done < <(find "$repos_dir" -type d -name _manifests)

if [ "$deleted" -gt 0 ]; then
  echo "deleted $deleted manifests, running garbage-collect"
  docker exec "$CONTAINER" registry garbage-collect --delete-untagged=false \
    /etc/docker/registry/config.yml
  # garbage-collect drops blobs from disk, but the registry caches blob
  # descriptors in memory (storage.cache.blobdescriptor). Without this restart
  # the running process keeps serving stale descriptors, and a later re-push of
  # a swept digest is answered "already exists" and silently skipped.
  echo "restarting registry to clear the in-memory blob descriptor cache"
  docker restart "$CONTAINER" >/dev/null
  sleep 5
else
  echo "nothing to delete"
fi

# Verify: every surviving tag must still resolve. A 404 here means retention ate
# an image that is still referenced, so fail loudly rather than let a deployment
# discover it later as ImagePullBackOff.
broken=0
while IFS= read -r manifests_dir; do
  repo="${manifests_dir#"$repos_dir"/}"
  repo="${repo%/_manifests}"
  tags_dir="$manifests_dir/tags"
  [ -d "$tags_dir" ] || continue
  while IFS= read -r tag; do
    [ -n "$tag" ] || continue
    code=$(curl -fsS -o /dev/null -w '%{http_code}' \
      -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
      -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
      "$REGISTRY_URL/v2/$repo/manifests/$tag" 2>/dev/null || echo 000)
    if [ "$code" != "200" ]; then
      echo "BROKEN: $repo:$tag does not resolve (HTTP $code)"
      broken=$((broken + 1))
    fi
  done < <(ls -1 "$tags_dir")
done < <(find "$repos_dir" -type d -name _manifests)

if [ "$broken" -gt 0 ]; then
  echo "FAIL: $broken tag(s) left dangling by retention"
  exit 1
fi
echo "ok: all remaining tags resolve"
