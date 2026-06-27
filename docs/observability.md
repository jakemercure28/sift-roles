# Observability

A self-hosted Prometheus + Grafana + Loki stack on the ThinkPad gives you the
health of both the infra (host + containers) and the app (scrape/score pipeline,
Gemini quota, HTTP latency, logs). It is one shared stack (compose project
`jsa-observability`) that monitors **both** the prod and staging app stacks.

## What runs

| Component | Image | Role |
|---|---|---|
| Prometheus | `prom/prometheus` | Scrapes + stores metrics (15d retention) |
| Grafana | `grafana/grafana` | Dashboards (provisioned), reached via the tunnel |
| Loki | `grafana/loki` | Log storage (filesystem, single-binary) |
| Promtail | `grafana/promtail` | Ships every container's JSON logs to Loki |
| node-exporter | `prom/node-exporter` | Host CPU / memory / disk / network |
| cAdvisor | `cadvisor` | Per-container CPU / memory / network / fs |

The app stacks expose metrics; this stack scrapes them:

- **Go backend** serves Prometheus metrics on a dedicated in-container listener
  (`METRICS_ADDR`, default `:9090`), kept off the dashboard mux so `/metrics` is
  never reachable through the public tunnel. It is published to the host loopback
  at `METRICS_PORT` (prod `9101`, staging `9102`).
- **scraper-service** serves `/metrics` on its Fastify port, published to the host
  loopback at `SCRAPER_METRICS_PORT` (prod `4041`, staging `4042`).

Prometheus joins both app Docker networks (`jsa-prod_default` and
`jsa-staging_default`) and scrapes app metrics by unique compose container name
(`jsa-prod-go-backend-1`, etc.) on the in-container ports. That is intentional:
on native Linux, a container cannot reach host-loopback-published ports through
`host.docker.internal`. The loopback port mappings remain useful for quick
`curl` checks on the ThinkPad. Exporters in this stack are scraped by service
name.

## App metrics (all prefixed `jsa_`)

| Metric | Type | Meaning |
|---|---|---|
| `jsa_scrape_cycles_total{result}` | counter | Scrape-insert cycles, ok/error |
| `jsa_scrape_cycle_duration_seconds` | histogram | Per-tenant scrape cycle time |
| `jsa_jobs_scored_total{result}` | counter | Jobs scored, ok/failed |
| `jsa_scoring_pass_duration_seconds` | histogram | ScoreUnscored pass time |
| `jsa_gemini_requests_total{operation,result,status}` | counter | Gemini HTTP generation attempts by path/status |
| `jsa_gemini_request_duration_seconds{operation,result,status}` | histogram | Gemini attempt latency |
| `jsa_gemini_tokens_total{operation,kind}` | counter | Gemini usageMetadata token counts (`prompt`, `cached_prompt`, `candidates`, `total`) |
| `jsa_gemini_daily_usage{user}` | gauge | Per-tenant Gemini calls today |
| `jsa_gemini_host_daily_usage` | gauge | Shared host-key calls today |
| `jsa_gemini_daily_tokens{user,kind}` | gauge | Per-tenant Gemini tokens today, persisted in `api_usage` |
| `jsa_gemini_host_daily_tokens{kind}` | gauge | Shared host-key Gemini tokens today |
| `jsa_jobs_pending_unscored{user}` | gauge | Scoring backlog per tenant |
| `jsa_http_request_duration_seconds{route,method,code}` | histogram | Dashboard request latency |
| `jsa_scraper_*` | various | Scraper duration, result counts, in-flight |

Plus the standard `go_*`, `process_*`, `node_*`, and `container_*` series.

## Dashboards

Provisioned into the **Sift** folder in Grafana:

- **Infra Health** — host CPU/mem/disk/load, per-container CPU/mem, host network.
- **App & Pipeline** — backlog, Gemini quota/tokens, scrape cycles, scoring
  throughput, scrape/score durations, HTTP rate + p95 latency. Has an `env`
  (prod/staging) variable.
- **Logs** — log volume by level + a live log panel, filterable by service and
  level. trace_id is a field (not a label); query it with
  `{service="go-backend"} | json | trace_id="<id>"`.

## Bring it up

On the ThinkPad:

```bash
cp observability/.env.example observability/.env   # set GRAFANA_* values
docker compose -p jsa-observability \
  -f observability/docker-compose.observability.yml up -d
```

Grafana binds `127.0.0.1:3000` only. Prometheus binds `127.0.0.1:9090` for local
debugging.

## Reaching Grafana (Cloudflare Tunnel + Access)

Grafana is loopback-only; the tunnel is the only way in, and Cloudflare Access is
the auth gate.

1. Add an ingress rule to the `cloudflared` config on the box:
   ```yaml
   ingress:
     - hostname: grafana.siftroles.com
       service: http://127.0.0.1:3000
     # ...existing rules, ending with the catch-all:
     - service: http_status:404
   ```
   Then `cloudflared tunnel route dns <tunnel> grafana.siftroles.com` and restart
   the tunnel.
2. In the Cloudflare Zero Trust dashboard, create an **Access application** for
   `grafana.siftroles.com` with a policy allowing only your email. Set
   `GRAFANA_ROOT_URL=https://grafana.siftroles.com` in `observability/.env`.

Grafana keeps its own admin login (`GRAFANA_ADMIN_PASSWORD`) as a second gate
behind Access.

## Verify

```bash
# 1. App metrics are live (run on the box):
curl -s localhost:9101/metrics | grep jsa_scrape_cycles_total
curl -s localhost:4041/metrics | grep jsa_scraper_scrape_duration_seconds

# 2. Prometheus sees every target as UP:
#    open http://127.0.0.1:9090/targets  (go-backend prod+staging, scraper
#    prod+staging, node-exporter, cadvisor)

# 3. Grafana dashboards render with live data, and Loki has logs:
#    open https://grafana.siftroles.com -> Sift folder
```

Trigger a scrape from the dashboard ("Scrape now") and watch
`jsa_scrape_cycles_total` and the App & Pipeline panels move.

## Not included (yet)

- **Alerting** (Alertmanager). Thresholds are visual on the dashboards for now;
  the stat panels go yellow/red at backlog, quota, CPU/mem/disk levels.
- **Distributed tracing** (OpenTelemetry). The app already propagates
  `X-Trace-ID` and stamps it on logs, so you can stitch a request across log
  lines in Loki without a trace backend.
- **redis-exporter** is commented out in the compose file (Redis lives in the app
  stacks on another network and isn't published). cAdvisor already reports the
  Redis container's CPU/memory.
