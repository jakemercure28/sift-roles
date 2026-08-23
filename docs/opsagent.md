# opsagent: the self-healing infra agent

`opsagent` receives alerts, investigates them, and either applies a narrow
pre-approved fix or files a GitHub issue. It is the only thing in this stack that
acts on an alert rather than recording it.

## Why it exists

Before it, alert delivery was dead end to end. `kube-prometheus-stack` shipped
with `alertmanager.enabled: false`, the compose Prometheus had no `alerting:`
block, and msmtp had been failing Gmail auth nightly since roughly 2026-07-24.
The rules in `observability/prometheus/alerts.yml` were good and fired into a UI
nobody was watching.

The concrete cost: on 2026-07-29 `jsa-prod/scraper-service` had been failing its
liveness probe with HTTP 503 for 24 hours, 79 readiness failures and 6 restarts
deep. Kubernetes did the only thing it knows how to do, restart the pod, and the
restarts could not help because the fix was a code change (#646, the `/livez`
probe split) already merged to `dev` and waiting on a manual promotion to `main`.
Nobody was told for a day.

That is the shape of what this closes:

- **Nothing was listening.** Good rules with no receiver are not alerts.
- **Blind restarts are not healing.** A liveness probe retries the same failure
  forever without ever asking why.
- **Diagnosis is the expensive part**, and it is the part a model is actually
  good at: read the alert, pull pod state and logs and metrics, correlate,
  decide whether this is a known-safe fix or something a human has to see.

## Shape

```
kps-prometheus ─┐
                ├─→ Alertmanager ─webhook→ opsagent ─→ Gemini (one call)
compose Prom  ──┘                              │            │
                                               │      evidence collected first
                                               │      allowlisted actions
                                               ↓
                                    GitHub issue (escalation)
                                    audit log + jsa_opsagent_* metrics
```

One Deployment in `monitoring`, running the `opsagent` subcommand of the
go-backend binary (so there is no second image to build). It serves the
Alertmanager webhook on `:8091/alert`, exposes `/metrics` on the same port, and
runs a periodic sweep on an in-process cron as a backstop for problems no rule
covers.

Escalations go to GitHub issues rather than email, because GitHub's own
notifications already reach Jake and do not depend on the broken msmtp path. They
dedupe on alert fingerprint: one issue per problem, commented on when it recurs.

## What it can do

**It does not call tools.** Every run collects the same fixed evidence first
(`internal/opsagent/evidence.go`): pod state and restart counts, deployments,
warning events, logs from unhealthy pods only, firing alerts from both
Prometheis with their annotations, Argo sync and health, and the commits on
`dev` that are not yet on `main`. That whole picture goes to the model in one
request, and verbatim into the escalation issue — so what a human reads is
exactly what the model saw.

State-changing, allowlisted:

| Action | What it does |
|---|---|
| `rollout_restart` | Stamps a deployment's pod template, as `kubectl rollout restart` does |
| `delete_pod` | Deletes one pod so its controller recreates it |
| `scale_deployment` | Sets replicas, bounded by `OPSAGENT_MAX_REPLICAS` |
| `argocd_sync` | Syncs an Argo Application so the cluster converges on git. Never prunes. Bounded by `OPSAGENT_SYNCABLE_APPS` |

**Everything else is escalation-only**, and the system prompt names the ones that
come up: clearing a wedged score batch (`server batch evict <name>`, which needs
database access the agent does not have), promoting `dev` to `main`, and anything
touching Postgres, Supabase, backups or the host.

The `main`..`dev` comparison is the most important thing collected. It answers
"is the fix already merged but not promoted?" — the #646 situation — and it is
gathered on **every** run rather than only when someone thinks to ask. The
prompt is explicit that a fix waiting on promotion is an escalation, never a
restart.

## What bounds it

Two layers, independent by design.

**The action gate** (`internal/opsagent/gate.go`) is enforced in Go. Every
state-changing operation routes through `perform`, which calls `checkAction`
before anything executes. The model returns an action *name* from a fixed enum;
it never executes anything itself, and a name with no handler does nothing
however it is prompted.

**RBAC** (`deploy/k8s/opsagent/rbac.yaml`) is the backstop, so a bug in the gate
still cannot exceed what the cluster grants. Read is cluster-wide (diagnosis
needs to see `monitoring` and `argocd` too). Write is a namespaced Role in
`jsa-prod` and `jsa-staging` only: `patch` on deployments and
deployments/scale, `delete` on pods. Deliberately absent, and worth keeping
absent: no `pods/exec`, no `secrets` at any verb, no create/delete on
deployments, no configmap writes.

**`argocd_sync` sits outside both of those**, which is worth stating plainly
rather than leaving to be discovered. RBAC does not bound it, because the sync
goes through the Argo API with a token instead of through this ServiceAccount.
The namespace scope does not bound it either, because every Argo Application
object lives in `argocd` no matter which namespace it deploys into. The only
thing bounding it is `OPSAGENT_SYNCABLE_APPS`, checked by `checkArgoApp`.

That list must never include `opsagent` or `root`. Arming lives in the opsagent
Deployment's env, so an agent able to sync its own Application could arm itself,
which defeats the whole point of that app being manual-sync; `root` is the
app-of-apps and reaches everything. `LoadConfig` refuses to start if either
appears, so this cannot be lost to a careless edit. If you want a harder bound,
scope the Argo API token with Argo's own RBAC as well.

Alongside those: runs are serialized in-process, each alert fingerprint has a
45-minute cooldown, and `OPSAGENT_DAILY_CALL_BUDGET` bounds model spend per day.
Having no tool loop removes a class of risk on its own — there is no sequence of
calls to run away, just one request and one gated action.

## What it costs

Nothing. It runs on the Gemini free tier using the same `GEMINI_API_KEY` the
scorer already uses, sealed into the `monitoring` namespace.

Free-tier quotas are **per model**, which is what makes this work: opsagent runs
on `gemini-3.6-flash` (20 requests/day) and falls back to
`gemini-3.5-flash-lite` (500/day) on a 429. The scorer runs on
`gemini-3.1-flash-lite`. They never draw from the same budget.

One investigation costs exactly **one** request, because evidence is gathered
deterministically rather than through a tool loop — see `internal/opsagent/gemini.go`.
A sweep that finds a healthy cluster costs **zero**: it never calls the model at
all. `OPSAGENT_DAILY_CALL_BUDGET` (default 15) is enforced locally before the
request goes out, so hitting the ceiling is a logged decision rather than a 429
at 3am, and the evidence is still escalated without a diagnosis.

## Bringing it up, in this order

Alertmanager is currently **disabled**, and that is deliberate. Bring the pieces
up in this order or you will repeat an outage that already happened once:

1. Sync the `opsagent` app (manual sync) and confirm the Deployment is Running
   and `opsagent.monitoring.svc.cluster.local` resolves. The sealed secret is
   already committed, so there is nothing to create first.
2. Only then set `alertmanager.enabled: true` in
   `deploy/argocd/apps/monitoring.yaml` and uncomment the `alerting:` block in
   `observability/prometheus/prometheus.yml`, in one change.

**Why the order matters.** The `monitoring` app is auto-sync and the `root` app
tracks `dev`, so merging an `alertmanager.enabled: true` to `dev` deploys
Alertmanager to **prod** within minutes with no separate decision. The `opsagent`
app it routes to is manual-sync, so it does not come up on its own.
Enabling Alertmanager first left prod with an Alertmanager retrying into
`no such host` and dropping a real firing alert every 10 minutes for ~17 hours.
The receiver must exist before anything is routed to it.

## Arming it

Writes are opt-in and off by default. Two env vars in
`deploy/k8s/opsagent/deployment.yaml` gate everything, and **both** must be set
before anything reaches the cluster:

```yaml
- name: OPSAGENT_DRY_RUN
  value: '0'                          # 1 (default) = nothing executes
- name: OPSAGENT_ENABLED_ACTIONS
  value: 'rollout_restart'            # empty (default) = every action dry-runs
```

An action that is understood but not armed comes back to the model as
`DRY RUN`, not `DENIED`. That is deliberate: the transcript records what the
agent wanted to do, which is the evidence you need in order to arm it. A denial
would hide the intent.

The intended path:

1. Deploy with the defaults. Read transcripts in the pod logs and in the
   escalation issues.
2. Judge whether the *diagnosis* is right and whether the proposed action is the
   one you would have run. Do not arm anything until several look right.
3. Arm one action, starting with `rollout_restart`, on staging.
4. Only then prod, still one action at a time.

## Operating it

```bash
# What is it doing?
ssh thinkpad sudo -n kubectl -n monitoring logs deploy/opsagent -f

# Is the alert path alive end to end?
ssh thinkpad sudo -n kubectl -n monitoring get pods -l app=alertmanager
ssh thinkpad sudo -n kubectl -n monitoring port-forward svc/kps-alertmanager 9093:9093

# Stop it immediately (the app is manual-sync, so selfHeal will not undo this)
ssh thinkpad sudo -n kubectl -n monitoring scale deploy/opsagent --replicas=0

# Hand it an alert by hand, to test without waiting for a real one
curl -XPOST localhost:8091/alert -d '{"status":"firing","alerts":[{"status":"firing",
  "labels":{"alertname":"TestAlert","namespace":"jsa-staging"},
  "annotations":{"summary":"synthetic"},"startsAt":"2026-07-29T00:00:00Z"}]}'
```

Its own escalation issues are labelled `opsagent`. Each one carries the model's
write-up, a table of every action considered (including dry runs and denials),
and the triggering alert.

## Watching the watcher

The agent must not become the component nobody watches, which is the failure it
exists to close. Two rules in `alerts.yml` cover it:

- `ChronicOpsAgentEscalation` — six or more escalations in 24h. Either the
  underlying fault genuinely needs a human and is silently repeating, or the
  agent is looping on something it cannot fix.
- `OpsAgentRunsFailing` — runs ending in `error`, `refused`, or
  `escalation_failed`. The last one matters most: it means the GitHub token
  cannot open issues, so findings are being lost.

Metrics: `jsa_opsagent_runs_total{trigger,verdict}`,
`jsa_opsagent_actions_total{action,namespace,outcome}`,
`jsa_opsagent_escalations_total`, `jsa_opsagent_tokens_total{direction}`,
`jsa_opsagent_run_duration_seconds`. Denials and dry runs are counted
deliberately: what the agent wanted to do is as informative as what it did.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `ANTHROPIC_API_KEY` | *(required)* | Claude API key |
| `OPSAGENT_DRY_RUN` | `true` | Nothing executes while set |
| `OPSAGENT_ENABLED_ACTIONS` | *(empty)* | Comma-separated actions armed to execute |
| `OPSAGENT_WRITABLE_NAMESPACES` | `jsa-prod,jsa-staging` | Namespaces actions may target |
| `OPSAGENT_MAX_REPLICAS` | `3` | Ceiling for `scale_deployment` |
| `OPSAGENT_MODEL` | `gemini-3.6-flash` | Diagnosis model. 20 req/day free |
| `OPSAGENT_FALLBACK_MODEL` | `gemini-3.5-flash-lite` | Tried once on a 429. 500 req/day free |
| `OPSAGENT_DAILY_CALL_BUDGET` | `15` | Local ceiling on model calls per day |
| `OPSAGENT_SYNCABLE_APPS` | `jsa-prod,jsa-staging` | Argo Applications `argocd_sync` may target. Must exclude `opsagent` and `root`, and the agent refuses to start if either appears |
| `OPSAGENT_MODEL` | `claude-opus-5` | Diagnosis model |
| `OPSAGENT_EFFORT` | `high` | `output_config.effort` |
| `OPSAGENT_TASK_BUDGET` | `60000` | Token budget per investigation (API min 20000) |
| `OPSAGENT_MAX_ITERATIONS` | `24` | Tool-loop ceiling |
| `OPSAGENT_PROM_IN_CLUSTER` | `http://kps-prometheus.monitoring.svc:9090` | k8s metrics |
| `OPSAGENT_PROM_COMPOSE` | `http://compose-prometheus.monitoring.svc:9090` | `jsa_*` metrics |
| `OPSAGENT_ARGOCD_SERVER` | `http://argocd-server.argocd.svc` | Argo API |
| `OPSAGENT_ARGOCD_TOKEN` | *(unset)* | Unset disables the Argo tools |
| `OPSAGENT_GITHUB_TOKEN` | *(unset)* | Unset disables escalation and commit tools |
| `OPSAGENT_GITHUB_REPO` | `jakemercure28/job-search-automation-private` | Escalation target |
| `OPSAGENT_LISTEN_ADDR` | `:8091` | Webhook and metrics |
| `OPSAGENT_SWEEP_SCHEDULE` | `17 */6 * * *` | Periodic sweep |

## Deploying it

The Argo app is **manual sync**, for two reasons. CI pushes code and the
image-tag bump as two commits, so an auto-syncing app can apply manifests
referencing an image tag that is not in the registry yet and land in a 404
crashloop; the agent needs an image that already carries the `opsagent`
subcommand. And arming is a judgement call that should not happen as a side
effect of a merge.

Before the first sync, produce the sealed secret: see
`deploy/k8s/opsagent/opsagent-secret.example.yaml`. Until it exists the
Deployment stays Pending on a missing secret, which is the correct failure mode.

```bash
argocd app sync opsagent
```
