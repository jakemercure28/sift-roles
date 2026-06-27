# Dashboard File Map

Which files to edit for what.

## Server

| File | Purpose |
|------|---------|
| `dashboard.js` | HTTP server entry point, routes to handlers |
| `lib/dashboard-routes.js` | All API endpoints, filter query logic, stats computation |
| `lib/dashboard-html.js` | Main page template, analytics view, scraper health |
| `lib/db.js` | SQLite initialization and helpers |

## HTML modules (`lib/html/`)

| File | Purpose |
|------|---------|
| `helpers.js` | COLORS, FILTER_DEFS, countBadge(), score/pipeline helpers |
| `filters.js` | Filter tab buttons (data-driven from FILTER_DEFS) |
| `job-rows.js` | Job table rows with action buttons |
| `modals.js` | All modal dialogs (outreach, help, notes, rejection, company) |
| `stats.js` | Header stats row (total, applied, interviewing, etc.) |

## Client-side

| File | Purpose |
|------|---------|
| `public/dashboard.js` | Modals, pipeline changes, search/filter, toasts, markdown rendering |
| `public/dashboard.css` | Full CSS with custom properties |
| `public/bookmarklet.template.js` | Source for the auto-fill bookmarklet |
| `scripts/build-bookmarklet.js` | Compiles the bookmarklet from the template + env vars |

## Pipeline

| File | Purpose |
|------|---------|
| `scraper.js` | Multi-platform job scraper |
| `pipeline.js` | Insert jobs into DB, score, deduplicate |
| `scorer.js` | Gemini API scoring, outreach drafting, rejection analysis |

## Config

The old root `config/*.js` and `lib/*.js` helpers have been retired. Logic now
lives in Go (`internal/...`) and the TypeScript `scraper-service/src/lib/`; the
only file left under `config/` is data.

| File | Purpose |
|------|---------|
| `config/metros.json` | Canonical metro list (embedded by the Go dashboard for the location filter) |
| `data/companies.json` | The active profile's company lists + search terms (written by the setup wizard) |

## Profile files (source of truth)

| File | Purpose |
|------|---------|
| `data/resume.md` | Polished resume |
| `data/context.md` | Career context, preferences, scoring criteria |
| `data/career-detail.md` | Deep project documentation per role |
| `data/experience/*.md` | Per-company experience files |
| `data/companies.json` | Target company lists by ATS platform |
| `.env` | Config + secrets (Gemini key, schedules, ports) |

## Common tasks

**Add a new filter tab:** Add entry to `FILTER_DEFS` in `helpers.js` → add query in `filterQueries` map in `dashboard-routes.js` → add stat to `globalStats` if it needs a count badge.

**Add a new modal:** Add HTML in `modals.js` → add open/close JS in `public/dashboard.js` → add CSS in `public/dashboard.css` → add Escape handler.

**Add a new API endpoint:** Add handler function in `dashboard-routes.js` → add route in `dashboard.js` → export from `dashboard-routes.js`.

**Add a new target company:** Add the slug to the appropriate array in `data/companies.json` (or use the `/add-company` command).
