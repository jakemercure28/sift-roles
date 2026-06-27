---
name: interview-prep
description: >
  Full interview prep for a specific role. Accepts a pasted job description, recruiter
  email, or just a company name. Generates a tailored "tell me about yourself", behavioral
  story answers, quick-reference match points, technical areas to review, questions to ask,
  and comp framing. Saves everything to the DB. Use when asked to "prep for an interview",
  "phone screen prep", "second round prep", "interview coaching", "what should I say",
  "practice interview", or "get ready for [company]".
version: 2.0.0
allowed-tools: Bash, Read
---

You are generating full interview prep for Jake. This covers both quick reference and spoken-answer coaching in one pass. Follow these steps exactly.

## Step 1: Parse the input

Read `$ARGUMENTS`. It may be a pasted job description, a recruiter email, or just a company name. Extract the company name and job title if present.

## Step 2: Resolve profile

Read `.env` to find `DATA_DIR_DB` and `DATA_DIR`. Defaults: `data/jobs.db` and `data`.

## Step 3: Find the job in the DB

```bash
node -e "
const db = require('better-sqlite3')(process.env.DATA_DIR_DB || 'data/jobs.db');
const jobs = db.prepare(\"SELECT id, title, company, score, stage, status FROM jobs WHERE status NOT IN ('archived', 'rejected') ORDER BY score DESC\").all();
console.log(JSON.stringify(jobs, null, 2));
"
```

Match the company name from the input to a job. If multiple matches, pick the highest score or most recent. Tell Jake which job you matched before proceeding.

Get the full record:

```bash
node -e "
const db = require('better-sqlite3')(process.env.DATA_DIR_DB || 'data/jobs.db');
const job = db.prepare(\"SELECT id, title, company, description, reasoning, stage FROM jobs WHERE id=?\").get('MATCHED_ID');
console.log(JSON.stringify(job, null, 2));
"
```

## Step 4: Read context files

Read all of these before generating any output:

- `data/resume.md` — specific numbers and company names to pull from
- `data/career-detail.md` — honest account of what was actually built at each job
- `.context/people/jake.md` — background, working style, preferences
- `.context/people/jake-voice.md` — writing rules (critical — read this carefully)

## Step 5: Generate prep

Generate all six sections. Sections marked "spoken" must follow the voice rules: no em dashes, no corporate buzzwords, noun-first sentences, vary sentence length, fragments allowed, no mic-drop endings.

---

### Quick Reference

2-3 bullets. Most relevant experience and standout numbers for this specific role. Fact-only, no prose.

### Match Points

For each major requirement area in the JD, 1-2 bullets:
- **JD area**: Company: specific achievement with number

---

### Tell Me About Yourself *(spoken)*

3-4 paragraph spoken answer tailored to this company and role. Lead with the most relevant experience. Pull real numbers. End low-key.

---

### Behavioral Questions *(spoken)*

Pick the 5 most likely behavioral questions based on the JD. For each:

**"[Question]"**
[Full spoken answer using a real story from career-detail.md. Specific company, situation, what Jake did, outcome with number. 3-6 sentences, conversational.]

Questions to consider (pick most relevant):
- A time you handled an incident under pressure
- A complex infrastructure project you owned end-to-end
- A time you made a decision with incomplete information
- A time you improved something nobody asked you to fix
- A time you had to learn something fast and ship it
- A time you disagreed with a decision and what you did
- How you make engineers around you more effective
- How you balance speed and reliability

---

### Technical Areas to Review

3-5 topics from the JD that Jake should brush up on. For each: his actual depth (strong/moderate/newer) and one concrete thing to know or practice. Skip anything he clearly knows cold.

---

### Questions to Ask

6-8 questions grouped by theme. Each should show he read the JD carefully and is thinking like someone who'd be doing this job.

Themes:
- Technical reality (current state, biggest pain, what's broken)
- Compliance/security ambitions (where are they actually vs. what the JD implies)
- Team structure (who he'd work with, on-call setup)
- Engineering culture (how decisions get made)
- Success definition (what does good look like in 6 months)

---

### Comp

Listed range (or "not listed"). Where Jake lands and why. One sentence on how to handle it if asked this round.

---

## Step 6: Save to DB

Write the full output to `/tmp/interview_notes.txt`, then:

```bash
python3 -c "
import sqlite3, os
notes = open('/tmp/interview_notes.txt').read()
db_path = os.environ.get('DATA_DIR_DB', 'data/jobs.db')
db = sqlite3.connect(db_path)
db.execute(\"UPDATE jobs SET interview_notes=?, updated_at=datetime('now') WHERE id=?\", (notes, 'MATCHED_ID'))
db.commit()
print('saved')
"
rm /tmp/interview_notes.txt
```

## Step 7: Confirm

Tell Jake: job matched, notes saved, visible via "View notes" on the dashboard. Show the full output inline. Offer to drill into any section or run a mock Q&A on a specific question.
