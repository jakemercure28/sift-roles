# Split-Stack Architecture (Go + TypeScript)

This repo runs as a split stack. **The Go engine scrapes, canonicalizes alternate ATS rows, scores, dedups/auto-ghosts, discovers companies, checks description quality, checks source ATS closure, validates ATS slugs, refreshes market research, syncs Gmail rejection emails, updates generated `.context` files, prunes logs, and serves the dashboard.** The deployed stack is now two services: `go-backend` plus the TypeScript `scraper-service`.

```
   cron (Go) ──▶ go-backend ──HTTP──▶ scraper-service (TS / Fastify)
                    │                    POST /scrape -> IdentifiedLead[]
                    ▼ (insert + score via Gemini)
            job_search_db (jobs.db, WAL) ◀── dashboard (Go)
```

## Services

| Service | Stack | Role |
|---|---|---|
| `scraper-service` | TypeScript + Fastify | `POST /scrape` returns validated `JobLead`s, each with its canonical `id`. Refuses to scrape until the profile is onboarded (`/health` reports `onboarded`). |
| `go-backend` | Go (`modernc.org/sqlite`, no CGO) | **Sole scraper, scorer, dashboard, and maintenance owner.** Cron (`SCRAPE_SCHEDULE`, default every 6h) → calls the TS scraper service → writes new `pending`/unscored rows + `scraped` events. Scoring (`SCORE_SCHEDULE`), canonicalize (`CANONICALIZE_SCHEDULE`), and maintenance (`MAINTENANCE_SCHEDULE`) run as Go crons. `SCRAPE_ON_START=1` scrapes once on boot. |

## Design decisions

- **Native TS scrapers (strangler complete).** All 12 scrapers are native TypeScript under `scraper-service/src/scrapers/*.ts`, registered in `src/bridge.ts`. They were migrated one at a time from the original CommonJS scrapers, each verified byte-identical by an offline fixture parity test (`scraper-service/test/*.parity.test.ts`). The CommonJS contract bridge has since been removed entirely: contract validation (`validateJobs`), the onboarding gate (`isOnboarded`), ATS detection (`detectAts`), id derivation (`deriveJobId`), and constants/company config are all native TypeScript under `scraper-service/src/lib/`. The only repo file the worker still reads at runtime is the active profile's `companies.json` (user data the wizard writes), loaded as plain JSON via `readProfileJSON()` in `src/lib/interop.ts`.
- **Single source of truth for job identity.** `deriveJobId` uses the WHATWG URL algorithm, which Go's `net/url` does not reproduce. Rather than reimplement it in Go (drift risk → duplicate rows), the TS worker emits the `id` and Go inserts it directly. Its golden fixtures (`scraper-service/test/fixtures/joblead/`) pin the output so the algorithm can't drift.
- **Scoped Go DB surface.** Go owns the scrape-write path (`InsertScrapedLead`, `LogEvent`, `ExistingJobKeys`), schema migration, scoring, canonicalization, maintenance, and generated context snapshots.
- **Shared SQLite, multiprocess-safe.** Go opens with WAL, `busy_timeout=5000`, `foreign_keys`, and `synchronous=NORMAL`.

## Running it

Both services start by default:

```bash
docker compose up -d        # scraper-service + go-backend dashboard
```

Local (no Docker):

```bash
cd scraper-service && npm install && npm start   # scraper service on :4040
SCRAPER_URL=http://localhost:4040 go run ./cmd/server scrape-once   # one scrape→insert cycle
go run ./cmd/server canonicalize-once               # resolve alternate ATS rows
go run ./cmd/server maintain-once                   # maintenance/context pass
SCORING_ENABLED=1 go run ./cmd/server score-once    # score unscored rows via Gemini
go test ./internal/...                              # Go unit tests
```

## The cutover (Node no longer scrapes or scores)

Go crons/subcommands replace the Node scrape (`scraper.js` + `pipeline.js`, which were `jobs.json`-batch-driven), canonicalizer, maintenance passes, and scorer with **DB-driven** equivalents that operate on whatever Go inserted:
- **Canonicalize** aggregator/alternate rows → primary ATS rows: owned by Go (`go-backend` canonicalize cron on `CANONICALIZE_SCHEDULE`, or `server canonicalize-once`).
- **Dedup** re-posts and duplicates + **auto-ghost** stale applied jobs + **company discovery** + **description checks** + **closed checks** + **slug validation** + **market research refresh** + **rejection email sync** + **context update** + **log pruning**: owned by Go (`go-backend` maintenance cron on `MAINTENANCE_SCHEDULE`, or `server maintain-once`; individual one-shots include `server rejection-sync-once` and `server context-update-once`). The three dedup SQL passes are ported verbatim from `dedupExistingJobs` (`lib/pipeline/import.js`) and auto-ghost/description/closed/log pruning from their Node scripts; discovery writes `suggested-companies.json` with the old TTL/bootstrap behavior, slug validation writes dashboard-compatible `slug-health.json`, market research reuses the native Go dashboard analyzer plus the old 23-hour cache TTL, rejection sync preserves the Gmail UID metadata/logging model, and context update writes generated `.context` snapshots. Git commits for `.context` changes are intentionally manual/local because the distroless Go service has no Git binary or `.git` mount.
- **Score** pending/unscored rows: owned by Go (`go-backend` with `SCORING_ENABLED=1`, on `SCORE_SCHEDULE`). The Node `retry-unscored.js` scorer is retired from the worker loop (`--skip-score`); it remains runnable manually.

### Validated on a clone of the production DB
A `VACUUM INTO`/read-only-mount copy of the live DB (5,736 jobs) was used to confirm: a full Go scrape returned 2,938 leads but inserted only **588** (the rest matched existing prod ids — `deriveJobId` parity holds vs real data); a second run inserted ~0 (idempotent); all inserts were `pending`/unscored with valid ids/urls; and `dedup-jobs.js` archived re-posts/duplicates exactly as the live pipeline does. The production volume was never written.

### Accepted, deliberate behavior (Go is more conservative)
- **No age-filter** (`/tmp/known_job_ids.json`) and **no reopen-for-rescore**: `INSERT OR IGNORE` skips ids already present; archived/rejected jobs stay archived (no resurrection of dead listings). Re-posts get a genuinely new id and a fresh `pending` row, then the dedup pass cleans up older duplicates — matching the existing pipeline.
