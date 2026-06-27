# Backend Architecture

A map of the Go backend for the job-search pipeline: what each piece does, how a
scrape becomes a scored job, and how the dashboard serves it.

For the storage layer in detail, see [database-architecture.md](./database-architecture.md).

## The split stack

The system is two processes plus a database:

```
┌──────────────────────────────────────────────────────────────────┐
│  go-backend  (cmd/server, single binary, many subcommands)        │
│                                                                    │
│  • dashboard HTTP server (static assets + page/JSON routes)        │
│  • cron schedulers (scrape, score, maintenance, canonicalize, …)   │
│  • LLM scorer (Gemini over HTTP)                                    │
│  • all database writes                                             │
└───────────┬──────────────────────────────────┬───────────────────┘
            │ HTTP /scrape                       │ SQL
            ▼                                    ▼
┌────────────────────────────┐       ┌──────────────────────────────┐
│  scraper-service (Node/TS)  │       │  database                     │
│  • Fastify worker           │       │  • SQLite (self-host)         │
│  • ATS scrapers             │       │  • Postgres/Supabase (hosted) │
│  • NO database access       │       └──────────────────────────────┘
└────────────────────────────┘
```

**Go owns** the dashboard, scoring, scheduling, maintenance, all DB writes, and
the `add-company` / `voice-check` CLI subcommands.
**Node owns** only the scraping tier: it fetches and parses ATS boards and returns
leads as JSON. It never touches the database. (Per the split-stack migration, the
old JS scraper tier and orchestration scripts are retired; the remaining root JS
is the CJS contract the Node worker `appRequire`s at runtime.)

## Entry point: `cmd/server`

One binary, dispatched by subcommand (`os.Args[1]`). Default (no subcommand)
starts the long-running service: dashboard + trigger server + cron schedulers.
The rest are one-shot CLI operations that run and exit.

| Subcommand | What it does |
|---|---|
| `server` *(default)* | Start dashboard, trigger server, and all crons |
| `scrape-once [plats]` | Run one scrape→insert cycle, then exit |
| `scrape-test [plats]` | Probe the worker only (no DB writes) |
| `score-once` | Score unscored jobs up to the daily quota |
| `canonicalize-once` | Resolve alternate ATS rows into canonical primary rows |
| `maintain-once` | DB-maintenance pass (dedup + auto-ghost) |
| `discover-once` | Discover + verify new company ATS boards |
| `add-company ...` | Verify + record one company's ATS board (`/add-company`) |
| `voice-check "text"` | Voice-check an application answer (`/app-questions`) |
| `descriptions-once` | Flag today's new jobs with short descriptions |
| `closed-check-once` | Check source ATSes for jobs that have closed |
| `slug-health-once` | Validate configured ATS board slugs |
| `rejection-sync-once` | Sync Gmail rejection emails into job stages |
| `context-update-once` | Write generated `.context` files from the DB |
| `market-research-once` | Refresh `market-research-cache.json` when stale |
| `prune-logs-once` | Delete old dated component logs |
| `dashboard` | Run the dashboard front door alone |
| `queue-worker` | Run the isolated Go/Asynq queue worker |
| `healthcheck` | Docker liveness: DB openable + worker reachable |

Every subcommand that touches data opens a repository via `mustOpenRepo` →
`openRepo`, which (on Postgres) scopes the base repo to the active tenant.

## Package map (`internal/`)

### Core plumbing
| Package | Responsibility |
|---|---|
| **`config`** | Loads runtime config from the environment; mirrors `config/paths.js` so Go and Node resolve the same paths. |
| **`db`** | The storage owner: `Repository`, dialect rewriter, embedded migrations, all queries. See the DB doc. |
| **`model`** | Shared domain types that cross service boundaries (the `JobLead` contract, byte-compatible with the TS worker's `JobLead`). |
| **`middleware`** | HTTP middleware, incl. per-request tenant resolution (`UserID(ctx)`). |
| **`auth`** | Verifies Supabase-issued JWTs against the project JWKS (hosted multi-tenant). |

### Scrape → insert
| Package | Responsibility |
|---|---|
| **`scraper`** | HTTP client to the TypeScript worker (`POST /scrape`). |
| **`scheduler`** | The scrape→insert cycle: ask the worker for leads, write the new ones, write the heartbeat. |
| **`trigger`** | Tiny in-cluster HTTP surface so the dashboard "Scrape now" button kicks off a scrape through Go (the only heartbeat writer). |
| **`discovery`** | Discover + verify new company ATS boards (`/add-company`), append to `companies.json`. |
| **`ats`** | ATS resolution/canonicalization helpers (Greenhouse/Ashby/Lever/Workday). |

### Score → enrich → maintain
| Package | Responsibility |
|---|---|
| **`scorer`** | Ports the Node Gemini scorer: scores listings against the candidate's resume/context via the Gemini API. |
| **`pipeline`** | Orchestrates post-scrape work: the scoring phase (bounded concurrency, daily quota, auto-archive low scores). |
| **`canonicalize`** *(in db)* | Folds alternate ATS rows into one canonical job. |
| **`closedcheck`** | Probes canonical ATS postings and closes rows whose posting is gone. |
| **`slughealth`** | Validates configured ATS board slugs (ports `validate-slugs.js`). |
| **`voice`** | Flags AI-flavored / off-voice text (ports `voice-check.js`). |
| **`contextupdate`** | Writes generated `.context` files from DB state. |
| **`rejectionsync`** | Syncs Gmail rejection emails into job stages. |
| **`logprune`** | Deletes old dated component logs. |

### Serving + async
| Package | Responsibility |
|---|---|
| **`dashboard`** | The whole web UI: static assets, health probe, every page and JSON route, served natively (the Node dashboard strangler migration is complete). 37 files. |
| **`tasks`** | Isolated Go queue tasks for the Asynq (Redis-backed) worker. |

## The daily data flow

```
   cron (ScrapeSchedule, ~8:07am)
        │
        ▼
   scheduler.Run
        │  scraper.Scrape ──HTTP──▶ scraper-service  ──▶ ATS boards
        │  ◀──────────── []JobLead (JSON) ──────────┘
        ▼
   for each lead: repo.InsertScrapedLead  (INSERT OR IGNORE, pending/unscored)
        │
        ▼
   repo.WriteHeartbeat(ok|error, scraped, inserted)   ← dashboard staleness signal
        │
        ▼
   cron (ScoreSchedule, gated by SCORING_ENABLED)
        │
        ▼
   pipeline scoring phase
        │  scorer.Score ──HTTP──▶ Gemini API
        │  writes score/reasoning, auto-archives low scores, tallies api_usage
        ▼
   maintenance / canonicalize / closed-check crons keep the set clean
        │
        ▼
   dashboard reads pending/scored rows for manual review
        │
        ▼
   human approves/rejects/advances → status & stage transitions + events row
```

Application submission and "applied" status are **human-controlled** — the
pipeline never auto-applies.

## Scheduling

The default `server` process starts cron jobs via `robfig/cron/v3`, each wrapped
in a **single-flight guard** so passes never overlap, and each stopped on context
cancellation:

- **scrape** — `ScrapeSchedule`
- **scoring** — `ScoreSchedule` (only when `SCORING_ENABLED`)
- **maintenance** — `MaintenanceSchedule`
- **canonicalize** — `CanonicalizeSchedule`

plus the description / closed / slug-health / rejection-sync / market-research /
log-prune passes. Heavier or isolatable work can run on the **Asynq** queue worker
(`queue-worker` subcommand, Redis-backed) instead of in-process.

## Dashboard request lifecycle

```
request → [auth middleware: verify Supabase JWT, set user sub in ctx]
        → Server.forRequest(r): clone scoped to ForUser(uid) + tenant dataDir
        → handler method (registered as method expressions, tenancy wired once)
        → Repository queries, all WHERE user_id = ?
        → HTML (server-rendered) or JSON response (gzip-aware)
```

- **Self-host:** no auth verifier, every request is `LocalUser`, no login UI. The
  page renders unauthenticated and hydrates client-side.
- **Hosted:** Supabase OAuth in the browser (supabase-js), bearer token on each
  request. Every tenant scores on the shared host Gemini key (`GEMINI_API_KEY`),
  bounded per tenant by `GEMINI_HOST_PER_TENANT_DAILY_LIMIT`.

Per-tenant profile files (`.context/people/applicant.md`, `voice.md`, etc.) are
sandboxed under `storage/users/{uid}/` on hosted; self-host keeps the repo-root
`.context`.

## Deployment

- Packaged as a **distroless** Docker image (`go-backend.Dockerfile`); the
  `healthcheck` subcommand is the liveness probe (no shell/curl in the image).
- `docker-compose.yml` runs `go-backend` + `scraper-service` (+ Redis for Asynq).
  `public/` and `lib/` are baked into the image, so UI edits require a rebuild
  (`docker compose up -d --build`) to appear.
- Config comes entirely from environment variables (`DATABASE_TYPE`,
  `DATABASE_URL`/`DB_PATH`, `SCRAPER_URL`, `SUPABASE_URL`, `SCORING_ENABLED`,
  schedule vars, `DB_*` pool tuning). See `.env.example`.

---

*One-line summary:* a single Go binary (`cmd/server`) orchestrates everything —
dashboard, schedulers, scorer, and all DB writes — while a stateless Node worker
does only the scraping; data flows scrape → insert → score → maintain → human
review, scoped per tenant at the application layer.
