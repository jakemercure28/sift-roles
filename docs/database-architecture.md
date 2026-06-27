# Database Architecture

How the job-search pipeline stores data and how the Go backend talks to it.

All database logic lives in **`internal/db/`**. The Go `cmd/server` process is the
only thing that opens a connection. The Node `scraper-service` has **no database
driver** at all; Go owns 100% of writes.

## One schema, two backends

The app runs against **either** backend, selected at startup by `DATABASE_TYPE`.
The same Go code serves both.

| | Self-host | Hosted (SaaS) |
|---|---|---|
| Backend | **SQLite** file (`data/jobs.db`, or `/app/db` volume in Docker) | **Postgres** (Supabase) |
| Tenancy | Single-tenant, every row owned by `'local'` | Multi-tenant, one partition per Supabase user `sub` |
| Driver | `modernc.org/sqlite` (pure Go) | `pgx/v5` via `database/sql`, simple-query protocol |
| Pooling | n/a | Bounded pool behind the Supavisor pooler (`:6543`) |
| Migrations | `internal/db/migrations/sqlite/` | `internal/db/migrations/postgres/` |
| Opened by | `db.Open(dbPath)` | `db.OpenPostgres(dsn, pool)` |

## The key design trick: write SQLite, run anywhere

Every query in the entire codebase is written **once, in SQLite dialect**. A
`dialect` rewriter (`internal/db/dialect.go`) translates on the fly at execution
time:

- **On SQLite:** `rewrite()` is the identity function. The self-host path and all
  tests run the literal queries, byte-for-byte unchanged.
- **On Postgres:** it rewrites the handful of SQLite-isms and rebinds placeholders:
  - `datetime('now', ...)`, `date(...,'localtime')`, `julianday(...)` → Postgres
    `to_char(... AT TIME ZONE 'utc', ...)` / epoch expressions
  - `INSERT OR IGNORE` → `INSERT ... ON CONFLICT DO NOTHING`
  - `?` placeholders → `$1, $2, ...` positional placeholders (skipping `?` inside
    string literals)

All access funnels through `Repository.exec / query / queryRow`
(`repository.go`), which call `dl.rewrite()` before hitting the driver. A
transaction wrapper (`execer`) does the same inside transactions.

Timestamp columns stay **TEXT** on both backends, storing the same
`'YYYY-MM-DD HH:MM:SS'` UTC strings SQLite writes, so Go's string-based time
handling (lexicographic ordering, RFC3339 parsing) is identical everywhere.

```
call site (SQLite-dialect SQL, "?" placeholders)
        │
        ▼
Repository.exec/query/queryRow
        │
        ▼
dialect.rewrite(query)  ──SQLite──▶ unchanged
        │
        └────────────────Postgres─▶ rewrite SQLite-isms + rebind ?→$N
        │
        ▼
database/sql driver  (modernc.org/sqlite | pgx)
```

## The Repository — the single access object

`Repository` (`internal/db/repository.go`) holds three things:

```go
type Repository struct {
    db     *sql.DB   // the open handle (SQLite file or Postgres pool)
    dl     dialect   // which backend; drives rewrite()
    userID string    // the tenant every query is scoped to
}
```

**Every tenant-owned query filters by `user_id = ?`** — even self-host, which
always uses the constant `LocalUser = "local"`. That means the multi-tenant query
shape is exercised even with a single user. The deliberate exceptions are global
market caches (`company_registry`, `global_jobs`), which do not carry user state.

- `ForUser(uid)` returns a cheap shallow clone scoped to a different tenant. This
  is the tenancy primitive used everywhere.
- `UserID()` / `DBType()` expose the current scope and backend.
- `ActiveTenant()` (Postgres only) returns the non-`local` user owning the most
  job rows — used by background workers that have no request to inherit from.
- `ProfileDir(base)` / `TenantDataDir(base, dt, uid)` map a tenant to its on-disk
  profile directory (`base/storage/users/{uid}/` on Postgres; `base` unchanged on
  self-host).

## Schema (11 tables)

The tenant-owned baseline starts in `migrations/{sqlite,postgres}/00001_baseline.sql`;
global caches and telemetry columns are added by follow-on migrations.

| Table | Purpose | Primary key |
|---|---|---|
| **`jobs`** | Tenant-owned job row: scraped/global lead → scored → pipeline-tracked (status, stage, score, reasoning, applied_at, rejection fields). Hosted duplicate market jobs use tenant-scoped `id` plus `global_job_id`. | `id` (global row id) |
| **`metadata`** | KV store: scraper heartbeat, schema_version, etc. | `(user_id, key)` |
| **`api_usage`** | LLM call and token tallies per day/model | `(user_id, date, model)` |
| **`company_notes`** | Per-company tags/notes | `(user_id, company)` |
| **`events`** | Audit trail of status/stage transitions (FK → jobs) | identity id |
| **`rejection_email_log`** | Gmail rejection-sync matches (FK → jobs) | identity id |
| **`job_aliases`** | Cross-platform dedup: alternate ATS rows → canonical job (FK → jobs) | `alternate_job_id` |
| **`status_snapshots`** | Daily pending/applied/interviewing counts | identity id |
| **`ats_resolution_cache`** | Cached ATS resolution outcomes per job | `job_id` |
| **`company_registry`** | Global cache of verified ATS boards discovered by any tenant | `(platform, registry_key)` |
| **`global_jobs`** | Global cache of scraped public job descriptions for cold-start tenant seeding | `id` |

Tables whose natural key repeats across users fold `user_id` into the PK. Job
rows keep a globally unique row id so foreign keys stay simple; `global_job_id`
preserves the scraper/global identity so multiple hosted tenants can each own
their own scored copy of the same public listing.

## Migrations (goose, embedded)

`internal/db/migrate.go` embeds the SQL via `go:embed` and runs
[goose](https://github.com/pressly/goose) at every `Open`. Three cases:

1. **Fresh DB** → goose runs the baseline, building the whole schema.
2. **Legacy Node DB** (no goose table but `metadata.schema_version >= 27`) → the
   baseline is *stamped as already-applied without replaying DDL*, leaving
   existing data untouched.
3. **Already goose-managed** → goose applies anything newer than the recorded
   version.

Postgres is always treated as fresh (no legacy Node history to stamp).

Current Postgres track:
- `00001_baseline.sql` — full schema, multi-tenant `user_id` baked in
- `00002_rls.sql` — row-level security policies (see below)
- `00003_perf_indexes.sql` — performance indexes
- `00004_company_registry.sql` — global verified-board cache
- `00005_api_usage_tokens.sql` — daily Gemini token counters
- `00006_global_jobs.sql` — global job-description cache + `jobs.global_job_id`

## Row-Level Security (dormant)

`00002_rls.sql` enables RLS and a `tenant_isolation` policy on the tenant-owned
tables, but it is **intentionally inert today**. The backend connects as the
Supabase service role (`BYPASSRLS`), so the real tenant guarantee is the
**application layer**: every tenant-owned query carries `WHERE user_id = ?`
(proven by a two-user isolation test). Global caches are intentionally excluded
from RLS because they store public market facts, not tenant state.

It is `ENABLE`, not `FORCE` — flipping to true DB-enforced isolation is a
deliberate future infra step: connect as a restricted non-bypass role and
`SET LOCAL app.user_id = '<sub>'` per request transaction, at which point each
policy matches the row's `user_id` against that GUC.

## Tenancy flow — a dashboard request

```
HTTP request
   │  Authorization: Bearer <supabase JWT>
   ▼
auth middleware  (internal/auth, internal/middleware)
   │  verify JWT signature against Supabase JWKS → put user `sub` in ctx
   ▼
Server.forRequest(r)   (internal/dashboard/server.go)
   ├─ clone.repo    = s.repo.ForUser(uid)      ← DB rows scoped to tenant
   └─ clone.dataDir = tenantDataDir(...)         ← profile files scoped to tenant
   ▼
handler runs every query against the tenant-scoped repo
```

On self-host there is no auth verifier, so `uid` is always `LocalUser` and
`dataDir` stays the repo root — that path is unchanged.

## Tenancy flow — background workers

Crons (scrape, score, maintenance, discovery, market research) run **outside any
HTTP request**, so they cannot inherit a tenant from middleware:

- `openRepo` (`cmd/server/main.go`) scopes the base repo to `ActiveTenant()` on
  Postgres — the dominant human user. Without this, scraped leads land in an
  orphan `'local'` partition the tenant's dashboard never sees.
- `ProfileDir(base)` resolves that tenant's on-disk profile dir via
  `TenantDataDir`, the single mapping shared by dashboard, workers, and the
  migrator.

## Write paths into `jobs`

All writes go through Go. The two insert entry points:

- **Daily scrape** (`internal/scheduler/scheduler.go`) — the scheduler calls the
  Node `scraper-service` over HTTP, gets leads back as JSON, and Go writes each
  via `Repository.InsertScrapedLead` (idempotent `INSERT OR IGNORE`, backfills an
  empty location/description on a pre-existing row). Each lead is also harvested
  into `global_jobs`.
- **Manual import** (`internal/dashboard/importjob.go`) — the `/add-job` path.

On user-triggered first runs, `Scheduler.RunOnceForUser` can seed up to
`GLOBAL_JOB_SEED_LIMIT` recent descriptions from `global_jobs` into the tenant's
pending queue before slower company discovery and scraping run. Scoring is still
per tenant because the score is resume × job.

Status/stage transitions, scoring, canonicalization, and analytics are
`Repository` methods spread across `internal/db/*.go`
(`dashboard.go`, `scoring.go`, `analytics.go`, `canonicalize.go`,
`marketjobs.go`, `insights.go`, …). Every transition also writes an `events` row
via `LogEvent`.

## Connection / pool settings

- **SQLite** (`Open`): DSN sets `busy_timeout(5000)`, `foreign_keys(on)`,
  `synchronous(normal)`; `journal_mode=WAL` is switched on once and persisted.
- **Postgres** (`OpenPostgres`): bounded pool (default 10 open / 5 idle,
  5-minute lifetimes; overridable via `DB_*` env vars). `pgx` is forced to the
  **simple query protocol** because the Supavisor transaction pooler does not
  preserve the server-side prepared statements `pgx`'s default mode relies on.

## Migration tool: `cmd/migrate-local`

One-shot lift-and-shift from self-host SQLite → hosted Postgres under one
Supabase uid. It reuses the production engine (`OpenPostgres` + the same dialect
rewriter via `Repository.Rewrite`), copies the tenant-owned history tables
jobs-first (so FKs to `jobs(id)` resolve), stamps `user_id` on each row, and is
re-run-safe via `ON CONFLICT DO NOTHING`. Runtime global caches
(`company_registry`, `global_jobs`) are intentionally rebuildable and are not
required for history migration. Identity ids are preserved with `OVERRIDING
SYSTEM VALUE` and the sequences are advanced past the migrated max afterward.

---

*One-line summary:* all DB logic is in `internal/db`, fronted by a single
`Repository` that writes SQLite-dialect SQL transparently rewritten to Postgres,
with tenancy enforced at the app layer (`WHERE user_id = ?` + `ForUser`) rather
than (yet) by the dormant Postgres RLS policies.
