---
description: Parse a pasted LinkedIn job description and create a record in the jobs database. Usage: /add-job [paste LinkedIn JD text]
allowed-tools: Bash, Read
---

You are importing a job from LinkedIn into the jobs database. Follow these steps exactly.

## Step 1: Parse the input

Read `$ARGUMENTS` — this is a raw LinkedIn job page paste. Extract the following fields:

| Field | How to find it |
|---|---|
| `title` | The job title (e.g. "Platform Engineer") — usually near the top before the company name |
| `company` | Company name (e.g. "NeuBird AI") — the line or element just after or before the title |
| `location` | Location string (e.g. "United States · Remote", "San Francisco, CA") — normalize dots/bullets to commas, note Remote separately |
| `url` | Any `linkedin.com/jobs/view/` URL in the paste — use `""` if not present |
| `posted_at` | "X days ago" / "X hours ago" / "X weeks ago" → convert to absolute ISO date (YYYY-MM-DD) using today's date from your context |
| `description` | Everything under "About the job" — full text, no truncation |
| `is_applied` | `true` if paste contains "Application submitted", "Applied", or "Applied X ago" / "Applied seconds ago" |

If the paste is ambiguous for any field, make a reasonable inference and note it in the confirmation summary.

## Step 2: Generate job ID and check for collision

Build the job ID by slugging company and title: lowercase, spaces to hyphens, strip all punctuation except hyphens.

Format: `linkedin-{company-slug}-{title-slug}`

Examples:
- NeuBird AI + Platform Engineer → `linkedin-neubird-ai-platform-engineer`
- Google + Senior SRE → `linkedin-google-senior-sre`

Check for an existing record:

```bash
curl -s "http://localhost:3131/api/dashboard-list" | node -e "
const chunks = [];
process.stdin.on('data', d => chunks.push(d));
process.stdin.on('end', () => {
  const data = JSON.parse(Buffer.concat(chunks));
  const jobs = Array.isArray(data) ? data : (data.jobs || []);
  const found = jobs.find(j => j.id === 'GENERATED_ID');
  console.log(JSON.stringify(found || null));
});
"
```

If a record exists, show it and ask: **keep existing** (abort), **update fields** (overwrite non-null fields), or **create with a suffix** (append `-2`). Do not proceed silently.

## Step 3: Insert and score via API

POST the job to the Go backend. It inserts into the live DB and scores inline using Gemini.

Build the JSON payload, escaping any special characters in the description. Then run:

```bash
curl -s -X POST http://localhost:3131/api/jobs \
  -H 'Content-Type: application/json' \
  -d '{
    "id": "GENERATED_ID",
    "title": "TITLE",
    "company": "COMPANY",
    "url": "URL_OR_EMPTY",
    "location": "LOCATION",
    "posted_at": "POSTED_AT",
    "description": "DESCRIPTION_ESCAPED"
  }'
```

Parse the JSON response:
- `inserted: true` — new row created; `inserted: false` — already existed (idempotent)
- `score` — 1-10 integer if scoring succeeded; absent if scorer was unavailable
- `reasoning` — one-line explanation from the scorer
- `error` — non-empty means the insert itself failed; show it and stop

## Step 4: Mark as applied (if detected)

If `is_applied` is `true` from Step 1, advance the job to applied status via the pipeline endpoint:

```bash
curl -s -X POST http://localhost:3131/pipeline \
  -H 'Content-Type: application/json' \
  -d '{"id": "GENERATED_ID", "action": "advance", "stage": "applied"}'
```

If that returns an error, note it but do not abort — the insert already succeeded.

## Step 5: Confirm

Print a summary like this:

```
Created: {Company} — {Title}
ID:      {job-id}
Score:   {score} — {one-line reasoning, or "pending (will score on next pipeline run)" if scorer was unavailable}
Status:  {pending|applied}
Location: {location}
Posted:  {posted_at}

Dashboard: http://localhost:3131
```
