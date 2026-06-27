---
name: load-context
description: >
  Load Jake's context files at the start of a job-search session.
  Use when asked to "load context", "read context", "get up to speed",
  or at the start of any job-search or career-related session.
version: 1.0.0
allowed-tools: Read, Bash
---

## Step 1: Find the active profile

Read `.env` and use `DATA_DIR` as the active profile directory.
Default to `data` if `DATA_DIR` is not set.
Use an ignored-file-aware listing command such as `rg --files -u {DATA_DIR}` or `find {DATA_DIR} -type f` so gitignored profile files are not missed.

## Step 2: Read session context

Read the following files in full and internalize them before responding:

1. `.context/people/jake.md` — Jake's background, skills, working style
2. `.context/people/jake-voice.md` — writing rules (critical for anything in Jake's voice)
3. `.context/projects/job-search.md` — pipeline architecture and features
4. `.context/reference/dashboard-files.md` — file map for what to edit
5. `.context/decisions/architecture.md` — why things are built this way

## Step 3: Read active profile source files

Read every human-readable source/context file under `{DATA_DIR}` recursively, especially:

- `data/context.md` — full career context, preferences, deal breakers
- `data/career-detail.md` — deep project documentation and honest career narrative
- `data/resume*.md` — current resume variants
- `data/experience/*.md` — per-company experience details
- `data/companies.json` — target company lists
- `data/dud-slugs.md` — known invalid ATS slugs
- `data/tailored-resumes/**/metadata.json` and `resume.md` — tailored resume context

Do not read binary or generated runtime artifacts as session context unless explicitly asked:

- `*.pdf`
- `*.db`, `*.db-shm`, `*.db-wal`
- `*.zip`
- `.DS_Store`
- `jobs.json`
- `market-research-cache.json`

After reading, confirm with one short line: what session context is loaded and you're ready.
