---
description: Add specific companies to the job-search pipeline by finding and verifying their ATS board (Greenhouse/Ashby/Lever/Workday). Usage: /add-company [company name(s) or a careers/board URL]
allowed-tools: Bash, Read, WebSearch, WebFetch
---

You are adding one or more named companies to the job-search pipeline. Discovery
finds companies automatically, but it relies on Gemini guessing boards and skews
toward DTC brands. This command is the manual path for companies the user names
directly (often large or legacy retailers on Workday). A company is only added
once its board verifies live, and the Go `add-company` subcommand (in the
go-backend) is the source of truth for that. Follow these steps.

## Step 1: Parse the request

Read `$ARGUMENTS`. It is a company name, a list of names, or a careers/board URL.
Build a list of target companies. If a usable board URL was pasted directly, you
can skip straight to Step 3 for that one.

## Step 2: Find each company's ATS board

For each company, find its real job board. Do NOT invent a URL. The verifier
will reject a wrong guess, so confirm before adding.

- **Workday** (most big-box and legacy retailers: Target, Gap, J.Crew, etc.).
  The board URL looks like `https://<tenant>.wdN.myworkdayjobs.com/<Board>`,
  where `wdN` is `wd1`..`wd12` and `<Board>` is a path segment (often the
  company name, `External`, or `Careers`). Find it by:
  - `WebSearch` for `<company> careers myworkdayjobs` or `<company> workday jobs`.
  - Or `WebFetch` the company's careers page and look for a link or redirect to a
    `*.wdN.myworkdayjobs.com` host. Copy the exact host and the first path
    segment after the locale (e.g. skip `en-US`).
- **Greenhouse / Ashby / Lever** (DTC/startups). Find the board slug:
  `boards.greenhouse.io/<slug>`, `jobs.lever.co/<slug>`, or
  `jobs.ashbyhq.com/<slug>`. You only need the slug.

## Step 3: Verify and add

Run the resolver once per company. It verifies the live board and only writes on
success. It is idempotent, so re-running is safe.

It runs inside the go-backend container so it writes to the same mounted
`data/suggested-companies.json` the scraper reads.

```bash
# Workday — pass the full board URL:
docker compose exec go-backend /server add-company --name "J.Crew" --url "https://jcrew.wd1.myworkdayjobs.com/jcrew"

# Greenhouse/Ashby/Lever — pass the slug (platform optional, it auto-detects):
docker compose exec go-backend /server add-company --name "Glossier" --slug glossier

# Or hand it a board URL and let it extract the slug:
docker compose exec go-backend /server add-company --name "Farfetch" --url "https://jobs.lever.co/farfetch"
```

Interpret the result:
- `Added: ...` — verified and written. Done.
- `Already tracked: ...` — it was already in the pipeline. No action needed.
- A non-zero exit with `add-company: ...` on stderr — the board did not verify.
  For Workday, the tenant, `wdN`, or board path is likely wrong: go back to
  Step 2, find the correct URL, and retry. For an API platform, the slug was
  wrong or the company is on Workday instead: try the Workday URL.

If a company genuinely is not on any of these four platforms (e.g. it uses
Taleo or SuccessFactors), say so and move on. Do not force it.

## Step 4: Confirm

Summarize what happened, for example:

```
Added (3):
  J.Crew        workday:jcrew
  Target        workday:target
  Glossier      greenhouse:glossier
Already tracked (1):
  Farfetch      lever:farfetch
Not on a supported ATS (1):
  Ann Taylor    (KnitWell/Ascena careers site, not Greenhouse/Ashby/Lever/Workday)
```

Newly added companies are scraped on the next scrape cycle (the go-backend
scrapes on its `SCRAPE_SCHEDULE`, and `data/` is bind-mounted into the container,
so no restart is needed). You can also trigger a scrape immediately with the
dashboard "Scrape now" button. Their jobs then flow through scoring like any
other listing.
