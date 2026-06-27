---
name: save-context
description: >
  Save what was learned this session back to .context/ files.
  Use at the end of a session, or when asked to "save context", "wrap up",
  "save what we learned", or "update context".
version: 1.0.0
allowed-tools: Read, Edit, Write, Bash
---

Review the conversation and persist anything worth keeping to `.context/`.

## Step 1: Review

Scan the conversation for anything that falls into these categories:

| If you noticed... | Update... |
|---|---|
| Jake corrected your approach or confirmed a non-obvious choice | `.context/people/jake.md` (working style section) |
| Jake mentioned a preference about how he works or communicates | `.context/people/jake.md` |
| Jake said something about career goals, priorities, or strategy | `.context/goals/jake-career.md` |
| Jake shared info about Natalie's career plans or situation | `.context/goals/natalie-career.md` |
| Jake mentioned move logistics, dates, housing, or timeline | `.context/goals/move.md` |
| An interview happened or Jake shared how one went | `.context/reference/interviews.md` |
| A meaningful career decision was made | `.context/decisions/career.md` |
| A new tool, service, or architectural choice was made | `.context/decisions/tooling.md` or `.context/decisions/architecture.md` |
| Jake mentioned a professional contact worth remembering | `.context/people/jake-network.md` |
| Something about a specific application (angle, referral, etc.) | `.context/reference/applications.md` |

All `.context/` files are in `/Users/jake/job-search/.context/`.

## Step 2: Write updates

For each file that needs updating:
- Read the current file first
- Add or update only what's new — don't duplicate what's already there
- Convert any relative dates to absolute dates (e.g. "next Thursday" → actual date)
- Don't copy things that already live in `data/` source files

If nothing meaningful was learned this session, say so and skip to done.

## Step 3: Commit

If any files were updated:

```bash
cd /Users/jake/job-search && git add .context/ && git commit -m "context: session update $(date +%Y-%m-%d)"
```

## Step 4: Report

Tell Jake what was saved (one line per file updated) or confirm nothing needed saving.
