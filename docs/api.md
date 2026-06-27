# Internal API Reference

This document covers the HTTP surface of the Go backend (`go-backend`). There are two independent HTTP servers:

| Service | Default address | Purpose | Exposed |
|---|---|---|---|
| **Dashboard** | `:3131` (`DASHBOARD_ADDR`) | The review UI plus all page/JSON routes | Published to the host (`localhost:3131`) |
| **Scrape trigger** | `:8090` (`TRIGGER_ADDR`) | On-demand scrape kickoff | Compose-internal only (no host port); reachable by service name, e.g. `http://go-backend:8090` |

Unless noted, every dashboard endpoint returns `Content-Type: application/json`. Request bodies are JSON and capped at **1 MiB** (setup routes use a 2 MiB cap). Empty bodies decode to zero-value fields rather than erroring.

---

## Conventions

### Headers (every dashboard response)

The dashboard wraps all routes in three middlewares. The following headers are stamped on **every** response (including 401s, static assets, and errors):

| Header | Value |
|---|---|
| `Content-Security-Policy` | `default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self' https: wss:; base-uri 'self'; frame-ancestors 'none'; object-src 'none'` |
| `X-Content-Type-Options` | `nosniff` |
| `X-Frame-Options` | `DENY` |
| `Referrer-Policy` | `strict-origin-when-cross-origin` |
| `X-Trace-ID` | 16-byte hex trace id, echoed from the request if a valid hex `X-Trace-ID` was sent, otherwise generated |

**Request header (optional):** `X-Trace-ID: <hex>`. This is propagated to the response and logs. Non-hex values are ignored and replaced.

### Authentication

Behavior depends on deployment mode:

- **Self-host (SQLite, single tenant):** no auth. Every request runs as the `local` tenant. No `Authorization` header needed.
- **Hosted (Postgres, multi-tenant):** every non-public route requires a Supabase bearer token.

```
Authorization: Bearer <supabase-access-token>
```

A valid token's subject (`sub`) becomes the request tenant; all reads/writes are scoped to that tenant. A missing/invalid token on a protected route returns:

**`401 Unauthorized`**
```json
{ "ok": false, "error": "unauthorized" }
```

**Public paths** (reachable without a token, so the browser can load and run the login flow): `GET /`, `GET /healthz`, `GET /report-problem`, `GET /api/auth/config`, and anything under `GET /public/`.

### Error shape

Most JSON handlers emit errors as:
```json
{ "error": "<message>" }
```
Setup/integration handlers instead use:
```json
{ "ok": false, "error": "<message>" }
```
The per-endpoint sections below note which shape applies and the exact status codes.

### Unmatched routes

There is no fallback proxy. Any path that does not match a registered route returns **`404 Not Found`**.

---

## Dashboard service (`:3131`)

### Public routes

#### `GET /healthz`
Liveness + DB probe.

- **`200 OK`**: `{ "ok": true }`
- **`503 Service Unavailable`**: `{ "ok": false, "error": "<db error>" }`

#### `GET /api/auth/config`
Bootstrap config so the browser can decide whether to load supabase-js.

- **`200 OK`**
```json
{ "authEnabled": true, "url": "https://xxxx.supabase.co", "anonKey": "..." }
```
`authEnabled` is `false` on self-host; `url`/`anonKey` are empty there.

#### `GET /public/{path}`
Static assets. Only `.css`, `.js`, `.pdf` are served; any other extension, a directory, a missing file, or a `..` traversal returns **`404`**.

- **`200 OK`** with `Content-Type` per extension, `Vary: Accept-Encoding`.
- `Cache-Control: public, max-age=31536000, immutable` when a `?v=` query param is present, otherwise `no-cache`.
- `.css`/`.js` are gzip-encoded (`Content-Encoding: gzip`) when the client sends `Accept-Encoding: gzip`.

#### `GET /report-problem`
Returns the static "report a problem" HTML page. `Content-Type: text/html`. Always **`200`**.

#### `GET /`
The full dashboard HTML page. `Content-Type: text/html`.

**Query params:** `filter`, `sort`, `q`, `minScore`, `page`, `analysisError`, and the location write-params `setMetro`, `setUnlisted`, `setRemote` (see `/api/dashboard-list`).

- **`200 OK`**: rendered HTML.
- **`500 Internal Server Error`**: `internal error` (plain text) on a render/DB failure.

---

### Jobs & pipeline

#### `GET /job-description?id=<jobId>`
- **`200 OK`**: the job record (JSON object).
- **`400`**: `{ "error": "id required" }`
- **`404`**: `{ "error": "not found" }`
- **`500`**: `{ "error": "<db error>" }`

#### `POST /archive`
Hide a job from the default views.

**Body:**
```json
{ "id": "job-123" }
```
- **`200 OK`**: `{ "ok": true }`
- **`400`**: `{ "error": "<decode error>" }`
- **`500`**: `{ "error": "<db error>" }`

#### `POST /pipeline`
Set a job's pipeline stage. On `applied` or `rejected`, a rejection-likelihood note is filled in asynchronously (best effort, no effect on the response).

**Body:**
```json
{ "id": "job-123", "value": "applied" }
```
**Allowed `value`:** `""` (clears stage), `applied`, `phone_screen`, `interview`, `onsite`, `offer`, `closed`, `rejected`, `ghosted`.

- **`200 OK`**: `{ "ok": true }`
- **`400`**: `{ "error": "bad pipeline value" }` or `{ "error": "<decode error>" }`
- **`500`**: `{ "error": "<db error>" }`

#### `POST /api/jobs`
Import a manually-added job (used by the `/add-job` skill) and score it inline.

**Body:**
```json
{
  "id": "linkedin-123",
  "title": "Senior Platform Engineer",
  "company": "Acme",
  "url": "https://...",
  "location": "Remote",
  "posted_at": "2026-06-01",
  "description": "..."
}
```
`id`, `title`, `company` are required. `url`, `location`, `posted_at`, `description` are optional. Imported jobs are tagged with ATS platform `linkedin`.

- **`200 OK`**
```json
{ "ok": true, "id": "linkedin-123", "inserted": true, "score": 8, "reasoning": "..." }
```
`score`/`reasoning` are present only if a scorer is configured and scoring succeeded. `inserted` is `false` if the id already existed.
- **`400`**: `{ "error": "invalid JSON: ..." }` or `{ "error": "id, title, and company are required" }`
- **`500`**: `{ "error": "<db error>" }`

#### `GET /api/dashboard-list`
HTML fragments for the list/report views (used for in-page navigation without a full reload).

**Query params:**
- `filter`: one of `all`, `not-applied`, `applied`, `interviewing`, `offers`, `rejected`, `closed`, `archived`, `ghosted`, `analytics`, `activity-log`, `market-research`. Invalid → `not-applied`.
- `sort`: `score`, `date`, `location-asc`, `location-desc`. Invalid → `score` (except `filter=rejected` with no `sort` → `date`).
- `q`: free-text search.
- `minScore`: integer, default `1`.
- `page`: integer, default `1`.
- `analysisError`: passthrough banner text.
- `setMetro`, `setUnlisted`, `setRemote`: when present, persist the location preference before rendering (see below).

- **`200 OK`**
```json
{ "ok": true, "url": "/?filter=...", "titleHtml": "...", "filtersHtml": "...", "mainHtml": "..." }
```
- **`500`**: `{ "error": "<db error>" }`

---

### Company notes

#### `GET /company-notes?company=<name>`
Case-insensitive lookup. A blank or unknown company returns empty strings (still `200`).

- **`200 OK`**: `{ "tags": "tag1,tag2", "notes": "..." }`
- **`500`**: `{ "error": "<db error>" }`

#### `POST /company-notes`
**Body:**
```json
{ "company": "Acme", "tags": "remote,fintech", "notes": "..." }
```
`company` is required (matched case-insensitively).

- **`200 OK`**: `{ "ok": true }`
- **`400`**: `{ "error": "company required" }` or `{ "error": "<decode error>" }`
- **`500`**: `{ "error": "<db error>" }`

---

### Location preferences

#### `GET /api/location-prefs`
- **`200 OK`**: `{ "prefs": { ... }, "metros": [ ... ] }`

#### `POST /api/location-prefs`
**Body:**
```json
{ "metros": ["seattle"], "includeUnknown": true, "remoteOnly": false }
```
- **`200 OK`**: `{ "ok": true, "prefs": { ... } }`
- **`400`**: `{ "error": "<decode error>" }`
- **`500`**: `{ "error": "<save error>" }`

---

### Status & progress

#### `GET /api/scraper-heartbeat`
- **`200 OK`**: `{ "heartbeat": <stored json or null> }`
- **`500`**: `{ "error": "<db error>" }`

#### `GET /api/scoring-progress`
- **`200 OK`**
```json
{
  "active": false,
  "scored": 120,
  "unscored": 8,
  "total": 128,
  "etaSeconds": 32,
  "latestScoreAt": "2026-06-12T18:04:00",
  "apiUsed": 142,
  "quotaExhausted": false,
  "newJobs24h": 5,
  "newCompanies24h": 1,
  "strongFits24h": 2,
  "lastScrapeAt": "2026-06-12T08:07:00",
  "lastScrapeStatus": "ok",
  "lastScrapeInserted": 5,
  "discoveryAt": "...",
  "discoveryAdded": 12
}
```
`lastScrape*` fields appear only when a heartbeat exists; `discovery*` only when a discovery report exists. `latestScoreAt`/`lastScrapeAt` are `null` when unset.
- **`500`**: `{ "error": "<db error>" }`

#### `GET /api/market-activity?period=<12w|26w|52w|all>`
New-roles-per-week trend. Unrecognized `period` → `26w`.
- **`200 OK`**: array of week rows.
- **`500`**: `{ "error": "<db error>" }`

---

### Analytics & market research

#### `GET /api/analytics/audit`
Analytics reconciliation data for API consumers.
- **`200 OK`**: `AnalyticsAudit` object (`asOf`, `thresholds`, `health`, `funnel`, `scoreCalibration`, `ats`, `activity`, `actions`, `contributors`, `definitions`, `warnings`).
- **`500`**: `{ "error": "<error>" }`

#### `GET /api/market-research/audit`
Per-skill / per-location reconciliation between the cached Gemini analysis and live rows.
- **`200 OK`**: `marketAudit` object (`sample`, `location`, `skills`, `gaps`, `warnings`).
- **`500`**: `{ "error": "<error>" }`

#### `GET /market-research`
Redirects to the rendered page.
- **`302 Found`** → `Location: /?filter=market-research`

#### `POST /market-research`
Force-refresh the market-research cache (runs Gemini), then redirect.
- **`302 Found`** → `/?filter=market-research` on success, or `/?filter=market-research&analysisError=<msg>` on failure (e.g. quota: "Gemini free-tier daily limit reached (500/day). Try again tomorrow.").

---

### Scrape trigger (dashboard side)

#### `POST /api/scrape-now`
Proxies to the trigger service (`SCRAPE_TRIGGER_URL`).

- **`200 OK`**: `{ "ok": true }` (a fresh scrape was accepted)
- **`200 OK`**: `{ "ok": false, "busy": true }` (a scrape was already running)
- **`503 Service Unavailable`**: `{ "ok": false, "error": "Scraper trigger not configured" }`
- **`502 Bad Gateway`**: `{ "ok": false, "error": "Scraper unreachable" }` or `{ "ok": false, "error": "Scraper returned <code>" }`

---

### In-app help assistant

#### `POST /api/ask`
Gemini-backed help bot, grounded only on built-in app knowledge.

**Body:**
```json
{ "question": "Why won't a job mark itself applied?" }
```
`question` is required, trimmed, **max 500 characters**.

- **`200 OK`**: `{ "answer": "..." }` on success.
- **`200 OK`**: `{ "answer": null, "error": "no-key" }` (no scorer/key), `"unavailable"` (Gemini error), or `"empty"` (blank answer). These soft-errors are intentionally `200`.
- **`400`**: `{ "error": "A question is required." }` or `{ "error": "That question is too long. Keep it under 500 characters." }`

---

### Integrations (rejection-email sync)

#### `GET /api/integrations/rejection-sync`
- **`200 OK`**
```json
{ "configured": true, "paused": false, "status": { ... }, "appliedLast7d": 3 }
```
`configured` reflects `GMAIL_EMAIL` + `GMAIL_APP_PASSWORD` being set; `paused` reflects `REJECTION_EMAIL_SYNC_DISABLED`; `status` is the last-run record or `null`.
- **`500`**: `{ "error": "<db error>" }`

#### `POST /api/integrations/rejection-sync/run`
Runs one sync synchronously (no body required; can take tens of seconds).
- **`200 OK`**: `{ "ok": true, "fetched": 10, "applied": 2, "ignored": 6, "unmatched": 2 }`
- **`409 Conflict`**: `{ "ok": false, "busy": true }` (a sweep is already in flight)
- **`503 Service Unavailable`**: `{ "ok": false, "error": "Sync not available in this process" }`
- **`500`**: `{ "ok": false, "error": "<error>" }`

---

### Setup wizard & settings

These read/write the profile files (`resume.md`, `context.md`, `companies.json`, `career-detail.md`, `experience/*.md`) and `.env`. Bodies use a **2 MiB** cap; malformed JSON decodes to an empty object rather than erroring.

#### `GET /api/setup/status`
- **`200 OK`**: `{ "resumeContent": "...", "contextContent": "...", "companiesContent": "...", "hasKey": true }`

#### `GET /api/setup/career`
- **`200 OK`**: `{ "careerDetail": "...", "applicant": "...", "voice": "...", "experience": [ { "name": "acme.md", "content": "..." } ] }`

#### `GET /api/settings/env`
Returns the editable allow-listed env settings (with type/min/max/hint metadata). Secret fields report only `set: true|false`, never the value.
- **`200 OK`**: `{ "ok": true, "settings": [ { "key": "GEMINI_RATE_DELAY_MS", "label": "...", "type": "int", "value": "5000", ... } ] }`

#### `POST /api/settings/env`
**Body:**
```json
{ "settings": { "GEMINI_RATE_DELAY_MS": "5000", "SCRAPE_SCHEDULE": "0 */6 * * *" } }
```
Only allow-listed keys are accepted: `GEMINI_RATE_DELAY_MS`, `GEMINI_DAILY_LIMIT`, `SCRAPE_SCHEDULE`, `AUTO_ARCHIVE_THRESHOLD`, `DISCOVER_CANDIDATE_COUNT`, `DISCOVER_TTL_HOURS`, `GHOSTED_AFTER_DAYS`, `GMAIL_EMAIL`, `GMAIL_APP_PASSWORD`, `REJECTION_EMAIL_SYNC_DISABLED`.
- **`200 OK`**: `{ "ok": true, "saved": ["GEMINI_RATE_DELAY_MS", "SCRAPE_SCHEDULE"] }`
- **`400`**: `{ "ok": false, "error": "Not allowed: <key>" }` or a validation message (e.g. `"GEMINI_DAILY_LIMIT must be a whole number"`, `"... must be >= 1"`).
- **`500`**: `{ "ok": false, "error": "<write error>" }`

#### `POST /api/setup/resume`
**Body:** `{ "content": "...", "format": "md" }`. If `format` is `pdf` and no Gemini key is set, returns `400 { "ok": false, "error": "no_key" }`.
- **`200 OK`**: `{ "ok": true }`
- **`500`**: `{ "ok": false, "error": "<write error>" }`

#### `POST /api/setup/profile`
Builds `context.md` + `companies.json` from wizard fields and writes an `.onboarded` marker.
**Body:** `{ "location": "...", "industry": "...", "titles": "...", "searchTerms": "...", "stack": "...", "salary": "..." }` (newline-separated lists).
- **`200 OK`**: `{ "ok": true }`
- **`500`**: `{ "ok": false, "error": "<write error>" }`

#### `POST /api/setup/companies`
**Body:** `{ "searchTerms": "...", "maxAgeDays": "20" }`. `maxAgeDays`, if present, must be an integer 1 to 365.
- **`200 OK`**: `{ "ok": true }`
- **`400`**: `{ "ok": false, "error": "Job freshness must be a whole number of days between 1 and 365" }`
- **`500`**: `{ "ok": false, "error": "<write error>" }`

#### `POST /api/setup/api-key`
Persists the Gemini key to `.env` and the process env. (Self-host only; in hosted mode use the tenant settings route below.)
**Body:** `{ "key": "AIza..." }`
- **`200 OK`**: `{ "ok": true, "usageReset": true, "rescoreStarted": false }`
- **`500`**: `{ "ok": false, "error": "<write error>" }`

#### `POST /api/setup/test-key`
Validates a Gemini key against the live API.
**Body:** `{ "key": "AIza..." }`
- **`200 OK`**: `{ "ok": true }` (accepted), or `{ "ok": false, "error": "Key rejected by Gemini (invalid or expired)" }`.
- **`400`**: `{ "ok": false, "error": "No key provided" }`

#### `POST /api/setup/extract-profile`
Uses Gemini to extract titles/stack/etc. from `resume.md`. Soft-fails to empty fields when no key/resume.
- **`200 OK`**: `{ "ok": true, "titles": "...", "searchTerms": "...", "stack": "...", "salary": ..., "industry": "..." }`

#### `POST /api/setup/run-refresh`
Kicks off the first scrape via the trigger service. Requires `resume.md`, `context.md`, `companies.json` to be non-empty.
- **`200 OK`**: `{ "ok": true, "runId": "wizard-...", "busy": false }`
- **`400`**: `{ "ok": false, "error": "Profile incomplete, missing: ..." }`
- **`502`**: `{ "ok": false, "error": "<trigger error>" }`

#### `POST /api/setup/career`
Writes any of `careerDetail`, `applicant`, `voice`.
**Body:** `{ "careerDetail": "...", "applicant": "...", "voice": "..." }` (all optional)
- **`200 OK`**: `{ "ok": true, "written": ["applicant", "voice"] }`
- **`500`**: `{ "ok": false, "error": "<write error>" }`

#### `POST /api/setup/experience`
Writes one `experience/<name>.md` file.
**Body:** `{ "name": "acme.md", "content": "..." }`. `name` must match `^[a-z0-9][a-z0-9-_]*\.md$`.
- **`200 OK`**: `{ "ok": true, "name": "acme.md" }`
- **`400`**: `{ "ok": false, "error": "Invalid experience file name (...)" }`
- **`500`**: `{ "ok": false, "error": "<write error>" }`

#### `POST /api/setup/experience/delete`
**Body:** `{ "name": "acme.md" }` (same name validation as above).
- **`200 OK`**: `{ "ok": true, "name": "acme.md" }`
- **`400`**: `{ "ok": false, "error": "Invalid experience file name (...)" }`

#### `POST /api/setup/career/structure`
Uses Gemini to turn pasted raw notes into structured career-detail markdown.
**Body:** `{ "raw": "..." }`
- **`200 OK`**: `{ "ok": true, "text": "..." }`
- **`400`**: `{ "ok": false, "error": "Paste some notes to structure first." }`
- **`502`**: `{ "ok": false, "error": "AI structuring failed. Try again." }`

---

## Scrape trigger service (`:8090`)

Tiny internal surface. Compose-network only, not published to the host. Responses carry `X-Trace-ID` but **not** the dashboard's security headers.

#### `GET /health`
- **`200 OK`**: empty body.
- **`405 Method Not Allowed`**: for non-GET.

#### `POST /scrape`
Starts a scrape cycle and returns immediately. An empty `{}` body is fine.
- **`202 Accepted`**: `{ "ok": true, "started": true }` (this call started a cycle)
- **`409 Conflict`**: `{ "ok": false, "busy": true }` (a cycle was already running)
- **`405 Method Not Allowed`**: `{ "ok": false, "error": "method not allowed" }` (non-POST)
