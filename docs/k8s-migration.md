# Kubernetes (k3s) Migration

Decision log and runbook for moving the app stacks from docker compose to a
single-node k3s cluster on the ThinkPad. GitOps via Argo CD. Supabase/Postgres
stays on docker compose permanently; pods reach it over the host.

Every host-side command run during this migration is recorded here. Repo files
under `deploy/` are the source of truth; host copies are made manually and
noted below.

## Decisions

| Topic | Decision | Why |
|---|---|---|
| Distro | k3s, pinned `v1.36.2+k3s1` | CNCF-certified, single binary, lowest overhead, own containerd (zero Docker interaction) |
| Migration order | staging first, prod later and gated | prod serves real tenants at siftroles.com |
| CD | Argo CD, app-of-apps | GitOps: CI builds, Argo syncs. Decouples deploys from the single runner |
| Manifests | Kustomize base+overlays for the app, Helm only for third-party charts | overlays mirror the compose prod/staging override model |
| Registry | local `registry:2` at `127.0.0.1:5000` | home uplink is slow; pushes stay on loopback, zero egress |
| Secrets | sealed-secrets | encrypted secrets live in git, Argo syncs them |
| Database | Supabase stacks stay on compose | pods reach Postgres via a selector-less Service + EndpointSlice pointing at the Tailscale IP |
| Traefik :443 | NOT exposed (`deploy/k3s/traefik-config.yaml`) | TLS terminates at Cloudflare; tailscaled owns `100.87.101.41:443` for `tailscale serve` and servicelb DNAT would hijack it |
| Replicas | both app Deployments `replicas: 1` + `Recreate`, always | two live copies double-scrape and double-spend Gemini (single-writer schedulers, RWO PVC) |

Single-box reality: one node means every k3s upgrade, kernel reboot, or
eviction is downtime. Limits and probes buy hygiene and predictable failure,
not availability.

## Phase 0: preflight audit (2026-07-14)

Hardware: ThinkPad X1 Carbon 7th, 12 threads, 15.3Gi RAM, 937G NVMe (620G free).
Swap 4G (2.8G in use; the box also runs immich/nextcloud/navidrome/vaultwarden
compose stacks and two Kong instances that eat ~1.2Gi combined).

- Ports 80, 5000, 6443, 10250, 2379-2380 free. `tailscaled` holds
  `100.87.101.41:443` (tailscale serve → localhost:8081). Traefik therefore
  exposes only :80.
- cgroup v2 (`cgroup2fs`), `net.ipv4.ip_forward=1`.
- No CIDR conflict: all docker networks are 172.17-172.28/16; k3s defaults
  10.42.0.0/16 (pods) and 10.43.0.0/16 (services) are untouched.
- Data dirs are tiny: staging `data/` 432K, prod `data/` 2.1M, `.context/` 80K
  each. Cutover rsync freeze is seconds.
- App container usage (docker stats snapshot): go-backend 20-30MiB idle,
  scraper-service ~300MiB / 28% CPU mid-scrape. Manifest requests/limits are
  sized from this.
- RAM budget for new components: k3s core ~700Mi, Argo CD ~600Mi requests,
  monitoring stack (Phase 7) ~1-1.5Gi. Fits in current headroom (~10Gi
  available).

Gate result: full plan approved (16GB machine), including kube-prometheus-stack
in Phase 7.

## Phase 1: k3s install

Repo files: `deploy/k3s/config.yaml`, `deploy/k3s/traefik-config.yaml`.

Host steps (run over `ssh thinkpad`):

```bash
sudo mkdir -p /etc/rancher/k3s /srv/jsa/k3s-storage
sudo cp deploy/k3s/config.yaml /etc/rancher/k3s/config.yaml
# traefik-config.yaml goes into the auto-apply manifests dir, BEFORE install
# so Traefik never binds :443 at all:
sudo mkdir -p /var/lib/rancher/k3s/server/manifests
sudo cp deploy/k3s/traefik-config.yaml /var/lib/rancher/k3s/server/manifests/
curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION="v1.36.2+k3s1" sh -s - server
```

Firewall (servicelb binds :80 on 0.0.0.0; today's posture is loopback-only):

```bash
sudo ufw allow from 127.0.0.0/8 to any port 80 proto tcp
sudo ufw allow from 100.64.0.0/10 to any port 80 proto tcp   # tailnet
sudo ufw allow from 10.42.0.0/16 to any port 80 proto tcp    # pod CIDR
# 6443 (kube-api) from tailnet only:
sudo ufw allow from 100.64.0.0/10 to any port 6443 proto tcp
```

Kubeconfig on the Mac:

```bash
ssh thinkpad 'sudo cat /etc/rancher/k3s/k3s.yaml' \
  | sed 's/127.0.0.1/100.87.101.41/' > ~/.kube/thinkpad.yaml
export KUBECONFIG=~/.kube/thinkpad.yaml
```

Verify: `kubectl get nodes` Ready; kube-system pods Running; `kubectl top
node`; `curl -sI http://127.0.0.1:80` on the host answers (Traefik 404); NO
listener appears on :443 beyond tailscaled; `free -h` and `docker ps` show the
compose stacks unaffected.

Rollback: `sudo /usr/local/bin/k3s-uninstall.sh` removes everything cleanly.

Upgrades are deliberate: re-run the installer with a new `INSTALL_K3S_VERSION`
pin, never track latest.

## Phase 2: local image registry

Repo files: `deploy/registry/docker-compose.yml`, `deploy/registry/gc.sh`,
`deploy/k3s/registries.yaml`.

Host steps:

```bash
sudo mkdir -p /srv/jsa/registry
docker compose -p jsa-registry -f deploy/registry/docker-compose.yml up -d
sudo cp deploy/k3s/registries.yaml /etc/rancher/k3s/registries.yaml
sudo systemctl restart k3s
# weekly GC (root crontab):
# 0 5 * * 0  /srv/jsa/staging/deploy/registry/gc.sh >> /var/log/jsa-registry-gc.log 2>&1
```

HTTP is acceptable strictly because the registry never leaves loopback.
Docker treats 127.0.0.1 registries as insecure-by-default, so the CI runner
pushes with zero daemon config.

Verify:

```bash
docker pull alpine:3.20 && docker tag alpine:3.20 localhost:5000/test:1
docker push localhost:5000/test:1
kubectl run regtest --image=localhost:5000/test:1 --restart=Never --command -- sleep 5
kubectl get pod regtest   # Running/Completed proves the containerd pull path
kubectl delete pod regtest
```

## Phase 3: app manifests (`deploy/k8s/`)

Kustomize base + staging/prod overlays. Layout and the decisions inside:

- `base/`: env-agnostic Deployments/Services/ConfigMap/PVC. Both app
  Deployments are `replicas: 1` + `Recreate` (hard rule: two live copies
  double-scrape and double-spend Gemini; the data volume is RWO). Probes hit
  HTTP `/healthz` (:3131) and `/health` (:4040); deliberately NOT the
  `/server healthcheck` subcommand, which round-trips Postgres and would
  restart-loop the pod on a DB blip. Both pods run uid/gid 1000 + fsGroup
  1000 (matches on-disk ownership, jake is uid 1000), read-only rootfs, all
  caps dropped. Memory limits only, no CPU limits (single node, requests
  handle scheduling, CPU limits would only throttle).
- `base/supabase-db-service.yaml`: selector-less Service; each overlay adds
  an EndpointSlice → `100.87.101.41:5433` (staging) / `:5432` (prod). DSNs
  use host `supabase-db` in both envs. Postgres itself never moves.
- Overlays carry namespace, ingress hosts, env knobs (ported from the box
  `.env` files 2026-07-14), and the image tags CI rewrites.
- Secrets are NOT in git: `jsa-secrets` (staging: DATABASE_URL,
  RLS_DATABASE_URL, ENCRYPTION_KEY, GEMINI_API_KEY, SUPABASE_ANON_KEY; prod
  adds GMAIL_EMAIL, GMAIL_APP_PASSWORD, HUGGINGFACE_API_KEY,
  SAPLING_API_KEY) becomes a SealedSecret in Phase 4.
- `go-backend.Dockerfile` now bakes `public/` into the image (was a compose
  bind mount; k8s has no checkout to mount from).

Verified 2026-07-14: `kubectl kustomize` renders both overlays;
`kubectl apply --dry-run=server -k` passes for both against the live
cluster; namespaces `jsa-staging`/`jsa-prod` created.

## Phase 4: Argo CD (`deploy/argocd/`)

Installed 2026-07-14 from the Mac (chart `argo/argo-cd` **10.1.3**, Argo CD
v3.4.5, pinned):

```bash
helm repo add argo https://argoproj.github.io/argo-helm
helm install argocd argo/argo-cd -n argocd --create-namespace \
  -f deploy/argocd/values.yaml --version 10.1.3
```

- App-of-apps under `deploy/argocd/apps/`: `root.yaml` (applied by hand
  once, tracks `dev`), `sealed-secrets` (chart 2.19.1, controller 0.38.4;
  NOTE the chart repo moved to `https://bitnami.github.io/sealed-secrets`
  in June 2026, the old bitnami-labs Pages URL 404s), `jsa-staging`
  (tracks `dev`, automated+prune+selfHeal), `jsa-prod` (tracks `main`,
  MANUAL sync until Phase 8).
- **Pre-cutover safety:** both overlays pin every Deployment to
  `replicas: 0` via a kustomization patch. Argo therefore syncs the full
  stack and the CI tag-bump loop is exercisable end to end, but no pod
  runs a scheduler next to the compose stacks (the double-scrape and
  double-spend hazard). The cutover PR deletes the patch.
- Repo access: dedicated read-only ed25519 deploy key `argocd-readonly`;
  the private key lives ONLY in the in-cluster `repo-jsa` secret
  (namespace argocd, labeled `argocd.argoproj.io/secret-type=repository`).
  Documented bootstrap exception to GitOps.
- UI: `kubectl port-forward -n argocd svc/argocd-server 8080:80` from the
  Mac; admin password in `argocd-initial-admin-secret`.
- Known trim gap: chart 10.x still deploys the applicationset-controller
  despite `applicationSet.enabled: false` (~50Mi; revisit or ignore).
- TODO before Phase 8: back up the sealed-secrets controller private key
  (`kubectl -n kube-system get secret -l sealedsecrets.bitnami.com/sealed-secrets-key -o yaml`)
  into the offsite sensitive-backup flow, alongside
  `/var/lib/rancher/k3s/server/token`.

## Phase 5: CI build-push + tag bump (`deploy.yml`)

Implemented 2026-07-14 as a third job, `build-push`, in
`.github/workflows/deploy.yml` (`needs: gate`, thinkpad runner):

1. `docker build` both images from the triggering SHA, tagged
   `localhost:5000/jsa/<name>:${GITHUB_SHA::12}`, `docker push` over
   loopback. Plain daemon layer cache, shared with the compose deploys;
   never `type=gha` cache on this box (#604).
2. Rewrite the `newTag:` lines in `deploy/k8s/overlays/staging` (dev
   pushes) or `overlays/prod` (main pushes) and push the commit
   `chore(deploy): bump <env> images to <sha> [skip ci]` back to the
   branch. The bump targets the current branch TIP (refetch + retry x3),
   while the image is built from the exact triggering SHA.

Decisions, resolving risk R8:

- **No PAT.** The repo is on the free plan, so there is no API-level
  branch protection on dev or main; promotion is governed by
  branch-guard.yml + the `production` environment branch policy. The
  workflow's own `GITHUB_TOKEN` (job-scoped `contents: write`) pushes the
  bump, and GITHUB_TOKEN pushes can never retrigger workflows, so
  recursion is structurally impossible. `[skip ci]` + a
  `paths-ignore: deploy/k8s/overlays/**` push filter are belt and braces.
- **Prod gating is Argo's manual sync, not the workflow.** Main pushes DO
  bump `overlays/prod` automatically (main itself only advances via the
  human-gated dev PR), but the jsa-prod app never applies it until a human
  runs `argocd app sync jsa-prod`. This also keeps dev/main merges clean:
  each branch's CI only ever rewrites its own overlay.
- **`sed`, not `kustomize edit set image`.** kustomize rewrites the whole
  kustomization.yaml in its own style (comments dropped, prettier check
  broken, the #605 failure mode) and the binary isn't on the runner. The
  sed is anchored to the `newTag:` lines and guarded by an exact-count
  check.

Verify (end to end, after merge): trivial dev push -> gate -> build-push
-> new tag in `curl localhost:5000/v2/jsa/go-backend/tags/list` -> bot
bump commit on dev touching only the staging overlay -> (once repo access
is configured) Argo auto-syncs jsa-staging to the new tag with everything
still at replicas 0.

## Phase 6: staging cutover (2026-07-14)

Pre-flight, all verified before the cutover:

- Staging Postgres published on `100.87.101.41:5433` via
  `/srv/jsa/supabase-staging/docker/docker-compose.override.yml`. Two
  latent traps fixed while doing it: `COMPOSE_FILE=docker-compose.yml` in
  that stack's `.env` had been silently ignoring the override file (now
  lists both), and the db's membership in `jsa-staging_default` was a
  manual `docker network connect` that the recreate dropped (staging
  /healthz went 503 until reattached; now declared in the override so it
  survives recreates). **Prod has the same two traps, fix during Phase 8
  pre-flight.**
- `jsa-secrets` SealedSecret in the staging overlay (5 keys; regeneration
  recipe in the file header). DSNs use host `supabase-db:5432` in compose
  AND in k8s, so values ported unchanged.
- **Argo CD 3.x gotcha:** default `resource.exclusions` include
  discovery.k8s.io/EndpointSlice, so Argo silently never applied our
  manual slice (Service resolved, zero endpoints, probe failed). Fixed by
  pinning `resource.exclusions` in `deploy/argocd/values.yaml` (default
  list minus EndpointSlice) + `helm upgrade` + application-controller
  restart.
- Probes from an in-cluster busybox pod: `100.87.101.41:5433` open AND
  `supabase-db:5432` open (full Service -> EndpointSlice path).

Cutover (order is a hard rule: compose stops BEFORE pods start):

1. `docker compose -p jsa-staging stop` (stop, not down; rollback intact)
2. Bind the PVC (local-path is WaitForFirstConsumer, so the volume dir
   does not exist until a pod mounts it): short-lived helper pod mounts
   `jsa-data`, then rsync from `/srv/jsa/staging/`: `data/ -> <pv>/data/`,
   `.context/ -> <pv>/context/`, `logs/ -> <pv>/logs/`, chown 1000:1000.
3. Merge the cutover PR: deletes the replicas:0 patch (Argo scales up) and
   gates the compose `deploy` job to main only (a dev merge must not
   restart the stopped compose staging stack).
4. In-cluster smoke: /healthz ok, dashboard HTML, `:9090/metrics` jsa_*.
5. `/etc/cloudflared/config.yml`: staging.siftroles.com -> Traefik
   `http://localhost:80` (Host header routes), restart cloudflared (brief
   blip on ALL tunnels incl. prod).
6. External smoke: `https://staging.siftroles.com/healthz`, dashboard,
   Google OAuth login (auth-staging still terminates at compose Kong,
   untouched).

Rollback (≤5 min): revert the cloudflared line to `:3132`, scale k8s to 0
(re-add the replicas patch or `kubectl scale`), `docker compose -p
jsa-staging start`. Decommission after 7 clean days: Argo continuously
Synced, ≥1 full GitOps deploy shipped, zero unexplained restarts, the
scheduled scrape ran, disk/RAM stable, one deliberate reboot test.

## Phase 7: observability (metrics)

Goal: close the blind spot Phase 6 opened. The compose Prometheus scraped
staging by container name, and those containers are stopped, so staging went
dark in Grafana the moment it moved to pods.

**The plan said two Prometheus datasources behind Grafana. It shipped as one.**
A second datasource would have left the app-pipeline dashboard's staging half
dark (every panel is pinned to the `prometheus` datasource uid) and required
editing 30 panel queries, AND it needed Grafana to reach into the cluster,
which means a NodePort and new inbound firewall surface. Instead the cluster
Prometheus **remote-writes** `jsa_*` to the compose Prometheus:

    k3s Prometheus ──remote_write(jsa_*)──> compose Prometheus <── scrape ── compose prod
                                                    ▲
                                              Grafana (unchanged)

- One datasource, zero dashboard edits, both envs in every panel.
- `alerts.yml` covers staging again for free, so the pipeline rules did NOT
  get ported to a `PrometheusRule` CR. Porting them now would double-alert
  (both Prometheus instances firing the same rule for staging). That port is
  Phase 8 work, when the compose stack retires.
- Push, not pull: pods reaching the host is already allowed by ufw
  (`10.42.0.0/16`), so this needs no new firewall rule. Same
  Service+EndpointSlice pattern as `supabase-db`.

Also trimmed against the plan, to avoid running things twice on one box:
- **No k8s Grafana.** Compose Grafana stays the single UI until Phase 8.
- **No k8s node-exporter.** The compose one already scrapes this same host.
  Consequence: every `node_*` rule group in the chart is disabled (they would
  be permanently no-data here), and the R3 disk alert went into
  `observability/prometheus/alerts.yml` instead of a `PrometheusRule` — which
  is better anyway, since it now covers the whole box, not just k8s.
- **No Alertmanager.** Still no receiver configured; alerts remain visible in
  Prometheus `/alerts` + Grafana. Unchanged from before, still an open item.
- k3s embeds the control plane in one process, so kubeControllerManager /
  kubeScheduler / kubeProxy / kubeEtcd scraping is disabled: on k3s they are
  dead targets and permanently-firing default alerts.

`env` label: k8s supplies no equivalent of the compose `static_configs` env
label, and the dashboards filter every panel on it. The ServiceMonitors derive
it from the namespace (`jsa-staging` -> `env="staging"`), which is why they
live in `base/` unpatched by either overlay. `job` needs no work: the operator
labels series with the Service name, which already matches the compose
`job_name`.

Left dark until Phase 8: per-container CPU/memory for staging pods on the
infra dashboard. Compose cAdvisor only sees Docker, not k3s containerd, and
the kubelet's cAdvisor uses `pod`/`container` labels where that dashboard
queries `name`. The host-level panels (CPU/RAM/disk) are unaffected and still
cover the soak criteria.

### Logs (Alloy -> the compose Loki)

Same shape as the metrics decision, for the same reason: promtail tails Docker
only, so staging's logs vanished at the Phase 6 cutover. Grafana Alloy runs as
a DaemonSet in the cluster and pushes pod logs into the **existing** compose
Loki, rather than standing up a second Loki as the plan suggested.

    Alloy (k3s pods) ──push──> compose Loki <──push── promtail (Docker)
                                    ▲
                              Grafana (unchanged)

Two shippers, one Loki, one datasource, no duplicate storage, no second Loki's
RAM. Again a push from pods to the host, so ufw needs no new rule.

**Promtail is EOL and deliberately NOT rewritten**, contrary to the plan's R1.
It retires with this compose stack at Phase 8, when Alloy takes over
everything; rewriting it now would risk prod's log pipeline for zero
functional gain.

Alloy reproduces promtail's label set exactly, including the level-extraction
regex and the pino-numeric mapping, so `level` keeps ONE value per severity
across both worlds. Two labels worth knowing about:

- `service` needed no trick: the compose service names (`go-backend`,
  `scraper-service`) are already the pod `app` label values.
- `compose_project` is a **compat alias** set to the namespace. The logs
  dashboard filters on `{compose_project=~"jsa-.*"}` and the namespaces happen
  to be `jsa-staging`/`jsa-prod`, so reusing it keeps that dashboard working
  unedited. It is a lie on a pod, so Alloy also sets the honest `namespace`
  label. Retire both it and the dashboard query at Phase 8.

### Observability CD was broken by Phase 6 (fixed here)

The stack used to be reconciled by the tail of a **staging** deploy, because
the staging checkout was the tree Prometheus bind-mounts. Phase 6 gated
compose deploys to main-only, so that leg stopped running and the stack
silently lost CD entirely: `/srv/jsa/staging` froze at `b9665a4` with nothing
to ever update it again.

It now has its own dev-tracking checkout and workflow job
(`scripts/deploy-observability.sh`, `observability` job in `deploy.yml`):

- NOT the prod/main tree: dashboards and rules are iterated on constantly and
  that would put every tweak behind a manual main promotion.
- NOT the staging tree: that is the Phase 6 rollback target, and git-resetting
  it would quietly rewrite the code you would be rolling back TO.

One-time host setup (done 2026-07-15):

```bash
git clone --branch dev git@github.com:jakemercure28/job-search-automation-private.git \
  /srv/jsa/observability
# .env is gitignored and does NOT come with the clone. Without it, `up -d`
# recreates Grafana with the default admin password and a broken root URL:
cp /srv/jsa/staging/observability/.env /srv/jsa/observability/observability/.env
```

The Prometheus bind mount moves from `/srv/jsa/staging/observability/prometheus`
to `/srv/jsa/observability/observability/prometheus`. The compose project name
is unchanged, so the named volumes (`jsa-observability_prometheus_data` etc.)
carry over and no TSDB/Grafana data is lost.

## Phase 8

Prod cutover, gated on explicit go. Preconditions include backup script
updates: live data moves to `/srv/jsa/k3s-storage/pvc-*/`, and the
sealed-secrets private key + `/var/lib/rancher/k3s/server/token` must join
the offsite backup set.

Phase 7 leaves these for Phase 8 to clean up:
- Port `alerts.yml` to a `PrometheusRule` CR and drop the remote_write.
- Move Grafana into the cluster, flip `grafana.siftroles.com` in cloudflared.
- Re-enable the k8s node-exporter once the compose one retires with the stack.

## Host-side command log

Chronological record of everything actually run on the box (append entries as
they happen):

- 2026-07-14: Phase 0 audit (read-only: nproc/free/df/ss/stat/ip route/docker
  network inspect/du/docker stats). No changes made.
- 2026-07-14: Phase 1 executed. Staged `config.yaml` →
  `/etc/rancher/k3s/config.yaml` and `traefik-config.yaml` →
  `/var/lib/rancher/k3s/server/manifests/` (pre-install so :443 never bound),
  installed k3s `v1.36.2+k3s1`. Node Ready in ~3 min, Traefik 404s on :80,
  tailscaled still sole owner of :443, all compose stacks unaffected, k3s
  baseline cost ~600Mi RAM. Kubeconfig copied to the Mac as
  `~/.kube/thinkpad.yaml` (server rewritten to the Tailscale IP).
  NOTE: ufw was found ACTIVE with default deny incoming + allow-all on
  tailscale0, so the planned :80/6443 allows were unnecessary. Still PENDING
  (Jake runs these; needed before pods reach host Postgres in Phase 6):
  `sudo ufw allow from 10.42.0.0/16 comment 'k3s pods'` and
  `sudo ufw allow from 10.43.0.0/16 comment 'k3s services'`.
- 2026-07-14: Phase 2 executed. `/srv/jsa/registry` created, registry up as
  compose project `jsa-registry` (compose file staged at
  `/tmp/registry-compose.yml`; re-up from the repo path once this PR merges),
  `registries.yaml` installed, k3s restarted. Verified end to end:
  `docker push localhost:5000/test:1` then a pod ran from that image
  (containerd pull via the mirror). Weekly gc.sh cron still PENDING (add
  after merge so the script has a stable path on the box).
- 2026-07-14: Phase 4 completed. Jake ran the repo-access script (deploy key
  `argocd-readonly` + in-cluster `repo-jsa` secret); root app hard-refreshed
  to Synced, child apps spawned, jsa-staging auto-synced the day's CI tag
  bump at replicas 0. jsa-prod stays Unknown ("app path does not exist")
  until deploy/k8s reaches main.
- 2026-07-14: Phase 6 pre-flight. Staging Postgres published on
  100.87.101.41:5433 (supabase-staging compose override; also fixed
  COMPOSE_FILE ignoring the override and made the db's jsa-staging_default
  attachment compose-managed after the recreate dropped the manual one and
  briefly 503'd staging /healthz). ufw pod-CIDR allows found already in
  place. Argo CD helm-upgraded (rev 2, same chart 10.1.3) to un-exclude
  EndpointSlice; application-controller restarted; supabase-db-1 slice
  applied. In-cluster probes green: pod->100.87.101.41:5433 and
  pod->supabase-db:5432 both open.
- 2026-07-15: Phase 7 (metrics). Found observability CD dead since the Phase 6
  main-only gate (staging tree frozen at b9665a4, nothing reconciling the
  bind-mounted config). Gave it a dedicated dev-tracking checkout at
  /srv/jsa/observability + its own workflow job; copied the gitignored
  observability/.env across by hand. Cluster Prometheus remote-writes jsa_* to
  the compose Prometheus rather than becoming a second Grafana datasource, so
  the app-pipeline dashboard and alerts.yml cover staging again unedited.
  k8s Grafana / node-exporter / Alertmanager deliberately NOT deployed.
- 2026-07-15: Phase 7 metrics VERIFIED live. Argo synced monitoring +
  monitoring-config; jsa-staging briefly OutOfSync because the ServiceMonitor
  CRD did not exist at its first dry-run, then self-healed on retry (expected
  ordering, no action). Chart trims confirmed on the live cluster: 9 active
  targets, no node-exporter/kubeControllerManager/kubeScheduler/kubeProxy/etcd,
  no Alertmanager CR, no DaemonSet. remote_write proven: the compose Prometheus
  now serves jsa_active_users for BOTH env="prod" (scraped) and env="staging"
  (remote-written), same job label, no prometheus/prometheus_replica labels.
  The new `observability` job reconciled the stack from /srv/jsa/observability
  on its first run; Grafana came through the tree move healthy with its root
  URL and admin password intact (named volumes are project-scoped, so the TSDB
  carried over).
- 2026-07-15: found and fixed a PRE-EXISTING dead disk metric. node-exporter
  runs --path.rootfs=/host and STRIPS that prefix, so the root fs is reported
  as mountpoint="/". The infra dashboard's "Root disk used %" panel matched
  mountpoint="/host" and had therefore been showing nothing since it was
  written, and the new R3 disk alerts inherited the bug by copying it. Both
  fixed; verified the corrected expr returns 1 series (65.3% free, correctly
  inactive). The box had NO working disk visibility at all before this.
- 2026-07-15: Alloy failed its first Argo sync (ComparisonError, app never
  created resources). The chart passes `configMap.content` through Helm's
  `tpl`, so it tried to render the Alloy config AS A HELM TEMPLATE and died on
  the level-mapping stage's Go template syntax
  (`unexpected "\" in operand`). NOTE `alloy fmt` and `alloy validate` BOTH
  pass on that config: the failure is purely in Helm's tpl layer, so validating
  the config proves nothing about the chart rendering. Fix: config moved to
  deploy/k8s/monitoring/alloy-config.alloy, generated into a ConfigMap by
  kustomize (disableNameSuffixHash, since the chart references it by static
  name), and consumed with `configMap.create: false`. Bonus: the config is now
  a real .alloy file that can be linted directly.
