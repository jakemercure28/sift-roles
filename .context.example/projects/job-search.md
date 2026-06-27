# Job Search Pipeline

Automated job scraping, LLM scoring, and a local dashboard for managing applications. Runs a single active profile, selected by `DATA_DIR`.

## Architecture

Three stages: **scrape, score, serve.**

### Scrape (`scraper.js`)

Multi-platform job scraper. Runs daily via cron. Pulls from Greenhouse, Lever, Ashby, Workday, Workable, Wellfound, Built In, RemoteOK, Jobicy, Arbeitnow, WeWorkRemotely. Target companies live in `data/companies.json` for the active profile.

### Score (`pipeline.js` + `scorer.js`)

Uses Google Gemini Flash to:
- Score each job 1 to 10 against the active profile's resume and context
- Detect application complexity (simple or complex)
- Generate reasoning explanations
- Draft outreach messages on demand

### Serve (`dashboard.js`)

HTTP dashboard on a configurable port (default 3131). Server-rendered HTML, no framework, no build step. Client-side JS for interactivity (modals, pipeline changes, search/filter).

## Profile selection

The active profile is selected by env vars:
- `DATA_DIR` — directory holding `resume.md`, `context.md`, `companies.json`, etc.
- `DB_DIR` — directory holding the SQLite database (a named Docker volume in the container)
- `DASHBOARD_PORT` — dashboard HTTP port (default 3131)

The Go backend runs the scrape/score/maintenance pipeline against whatever `DATA_DIR`/`DB_DIR` point at.

## Key features

- Filter tabs: All, Not Applied, Applied, Follow-up, Interviewing, Need Outreach, Quick Apply, Rejected, Analytics, Archived
- Filter definitions centralized in `FILTER_DEFS` in `lib/html/helpers.js`
- Pipeline tracking (Applied → Phone Screen → Interview → Onsite → Offer / Rejected)
- AI-drafted outreach and follow-up messages
- Interview prep notes
- Rejection analysis with transcript attachment
- Company tags and notes
- Auto-fill bookmarklet for Greenhouse, Ashby, Lever forms
- Analytics: pipeline funnel and score calibration

## Profile source files

All of the applicant's career context, resume, experience details, and target company lists live in `data/`. These files are the source of truth for scoring, outreach drafting, and interview prep.
