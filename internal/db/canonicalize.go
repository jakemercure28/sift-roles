package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// This file ports the DB merge/alias layer of the ATS canonicalization pipeline
// from lib/db.js (canonicalizeAlternateJob and its helpers). When an aggregator
// ("alternate") job is resolved to a primary ATS posting, the alternate's
// pipeline state is folded into the canonical row, dependent rows are re-keyed,
// the mapping is recorded in job_aliases, and the alternate is archived. The
// network resolution that produces a Resolution lives in internal/ats.

// The pipeline-state columns carried across a merge are JOB_STATE_COLUMNS in
// lib/db.js: score, reasoning, outreach, status, first_seen_at, applied_at,
// stage, notes, reached_out_at, interview_notes, apply_complexity,
// rejected_from_stage, rejected_at, claude_score, claude_reasoning,
// rejection_reasoning, score_attempts, last_score_attempt_at, and score_error.
// Their order drives canonicalInsertColumns below.

// Job is a full jobs row, with NULL-able columns modeled so the blank/COALESCE
// merge semantics in lib/db.js port faithfully (blank == NULL or empty string).
type Job struct {
	ID          string
	Title       sql.NullString
	Company     sql.NullString
	URL         sql.NullString
	Platform    sql.NullString
	Location    sql.NullString
	PostedAt    sql.NullString
	ScrapedAt   sql.NullString
	Description sql.NullString
	CreatedAt   sql.NullString
	FirstSeenAt sql.NullString

	Score              sql.NullInt64
	Reasoning          sql.NullString
	Outreach           sql.NullString
	Status             sql.NullString
	AppliedAt          sql.NullString
	Stage              sql.NullString
	Notes              sql.NullString
	ReachedOutAt       sql.NullString
	InterviewNotes     sql.NullString
	ApplyComplexity    sql.NullString
	RejectedFromStage  sql.NullString
	RejectedAt         sql.NullString
	ClaudeScore        sql.NullInt64
	ClaudeReasoning    sql.NullString
	RejectionReasoning sql.NullString
	ScoreAttempts      sql.NullInt64
	LastScoreAttemptAt sql.NullString
	ScoreError         sql.NullString
}

// ResolvedJob is the canonical primary posting an alternate resolves to. It
// carries only content fields (no pipeline state); the alternate supplies the
// state on insert. Mirrors resolution.job in lib/ats-resolver.js.
type ResolvedJob struct {
	ID          string
	Title       string
	Company     string
	URL         string
	Platform    string
	Location    string
	PostedAt    string
	ScrapedAt   string
	Description string
}

// Resolution is the outcome of resolving an alternate job, mirroring the object
// returned by resolution() in lib/ats-resolver.js. Status is one of "primary",
// "unsupported", or "unresolved" (anything non-"primary" records an alias only).
type Resolution struct {
	Status     string
	Platform   string
	URL        string
	Job        *ResolvedJob
	Confidence float64
	Evidence   map[string]any
}

// CanonicalizeResult reports what CanonicalizeAlternateJob did. CanonicalID is
// set only when an alternate was merged into a primary row.
type CanonicalizeResult struct {
	Action      string // "canonicalized" | "unsupported" | "unresolved"
	CanonicalID string
}

// sqlExec is satisfied by both *sql.DB and *sql.Tx, so the helpers below work
// inside or outside a transaction.
type sqlExec interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// CanonicalizeAlternateJob folds a resolved alternate job into its canonical
// primary row, ported from canonicalizeAlternateJob in lib/db.js. A non-primary
// resolution records an alias only (and archives + logs an event when the URL is
// unsupported); a primary resolution inserts/backfills the canonical row, merges
// the alternate's pipeline state, re-keys dependent rows, records the alias, and
// archives the alternate, all in one transaction.
func (r *Repository) CanonicalizeAlternateJob(alt *Job, res Resolution) (CanonicalizeResult, error) {
	if alt == nil || alt.ID == "" {
		return CanonicalizeResult{}, fmt.Errorf("CanonicalizeAlternateJob requires an alternate row with id")
	}

	tx, err := r.db.Begin()
	if err != nil {
		return CanonicalizeResult{}, err
	}
	defer tx.Rollback()
	ex := execer{raw: tx, dl: r.dl}

	if res.Status != "primary" || res.Job == nil || res.Job.ID == "" {
		status := res.Status
		if status == "" {
			status = "unresolved"
		}
		if err := r.recordJobAlias(ex, alt, res, status, ""); err != nil {
			return CanonicalizeResult{}, err
		}
		if res.Status == "unsupported" {
			if err := r.archiveAlternateJob(ex, alt.ID, ArchiveReasonUnsupported); err != nil {
				return CanonicalizeResult{}, err
			}
			if err := logEventTx(ex, r.userID, alt.ID, "ats_resolution", strOrEmpty(alt.Platform), "unsupported"); err != nil {
				return CanonicalizeResult{}, err
			}
		}
		if err := tx.Commit(); err != nil {
			return CanonicalizeResult{}, err
		}
		return CanonicalizeResult{Action: status}, nil
	}

	// canonID is the tenant-local jobs.id for the resolved posting. On hosted
	// Postgres it is the per-tenant hashed row id (the raw resolved id is stored in
	// global_job_id), matching what a direct scrape of the same posting computes so
	// the two dedup; on SQLite/local it is the resolved id unchanged.
	canonID := r.JobRowID(res.Job.ID)
	if err := r.insertCanonicalJob(ex, res.Job, alt, canonID); err != nil {
		return CanonicalizeResult{}, err
	}
	if err := r.mergeAlternateState(ex, alt, canonID); err != nil {
		return CanonicalizeResult{}, err
	}
	if err := r.rekeyDependentRows(ex, alt.ID, canonID); err != nil {
		return CanonicalizeResult{}, err
	}
	if err := r.recordJobAlias(ex, alt, res, "primary", canonID); err != nil {
		return CanonicalizeResult{}, err
	}
	if err := r.archiveAlternateJob(ex, alt.ID, ArchiveReasonCanonicalized); err != nil {
		return CanonicalizeResult{}, err
	}
	if err := logEventTx(ex, r.userID, canonID, "ats_resolution", strOrEmpty(alt.Platform), res.Platform); err != nil {
		return CanonicalizeResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CanonicalizeResult{}, err
	}
	return CanonicalizeResult{Action: "canonicalized", CanonicalID: canonID}, nil
}

// canonicalInsertColumns is the INSERT column list from insertCanonicalJob in
// lib/db.js: the content columns, status, then the remaining state columns in
// jobStateColumns order (status excluded since it's listed explicitly).
var canonicalInsertColumns = []string{
	"id", "global_job_id", "title", "company", "url", "platform", "location",
	"posted_at", "scraped_at", "description", "status",
	"score", "reasoning", "outreach", "first_seen_at", "applied_at", "stage",
	"notes", "reached_out_at", "interview_notes", "apply_complexity",
	"rejected_from_stage", "rejected_at", "claude_score", "claude_reasoning",
	"rejection_reasoning", "score_attempts", "last_score_attempt_at", "score_error",
}

// insertCanonicalJob inserts the canonical row (inheriting the alternate's
// content and pipeline state) and then backfills any blank content columns on a
// pre-existing canonical row. Ported from insertCanonicalJob/buildCanonicalInsertRow.
func (r *Repository) insertCanonicalJob(ex sqlExec, job *ResolvedJob, inherited *Job, canonID string) error {
	// Content columns: prefer the resolved job, fall back to the alternate.
	title := firstNonEmpty(job.Title, strOrEmpty(inherited.Title))
	company := firstNonEmpty(job.Company, strOrEmpty(inherited.Company))
	url := firstNonEmpty(job.URL, strOrEmpty(inherited.URL))
	platform := firstNonEmpty(job.Platform, strOrEmpty(inherited.Platform))
	location := firstNonEmpty(job.Location, strOrEmpty(inherited.Location))
	postedAt := firstNonEmpty(job.PostedAt, strOrEmpty(inherited.PostedAt))
	scrapedAt := firstNonEmpty(job.ScrapedAt, strOrEmpty(inherited.ScrapedAt))
	description := firstNonEmpty(job.Description, strOrEmpty(inherited.Description))

	status := firstNonEmpty(strOrEmpty(inherited.Status), "pending")
	firstSeen := firstSeenInherit(inherited)
	scoreAttempts := int64(0)
	if inherited.ScoreAttempts.Valid {
		scoreAttempts = inherited.ScoreAttempts.Int64
	}

	values := []any{
		r.userID,
		canonID, job.ID, title, company, url, platform, location,
		postedAt, scrapedAt, description, status,
		intOrNil(inherited.Score),       // score (?? null)
		nullStr(inherited.Reasoning),    // reasoning (|| null)
		nullStr(inherited.Outreach),     // outreach
		nullable(firstSeen),             // first_seen_at (|| created_at || null)
		nullStr(inherited.AppliedAt),    // applied_at
		nullStr(inherited.Stage),        // stage
		nullStr(inherited.Notes),        // notes
		nullStr(inherited.ReachedOutAt), // reached_out_at
		nullStr(inherited.InterviewNotes),
		nullStr(inherited.ApplyComplexity),
		nullStr(inherited.RejectedFromStage),
		nullStr(inherited.RejectedAt),
		intOrNil(inherited.ClaudeScore),
		nullStr(inherited.ClaudeReasoning),
		nullStr(inherited.RejectionReasoning),
		scoreAttempts, // score_attempts (?? 0)
		nullStr(inherited.LastScoreAttemptAt),
		nullStr(inherited.ScoreError),
	}

	cols := append([]string{"user_id"}, canonicalInsertColumns...)
	placeholders := ""
	for i := range cols {
		if i > 0 {
			placeholders += ", "
		}
		placeholders += "?"
	}
	insertSQL := fmt.Sprintf(
		"INSERT OR IGNORE INTO jobs (%s) VALUES (%s)",
		joinCols(cols), placeholders,
	)
	if _, err := ex.Exec(insertSQL, values...); err != nil {
		return fmt.Errorf("insert canonical job: %w", err)
	}

	// Backfill blank content columns on the (possibly pre-existing) canonical row.
	_, err := ex.Exec(`
		UPDATE jobs
		SET title = COALESCE(NULLIF(title, ''), ?),
		    company = COALESCE(NULLIF(company, ''), ?),
		    url = COALESCE(NULLIF(url, ''), ?),
		    platform = COALESCE(NULLIF(platform, ''), ?),
		    location = COALESCE(NULLIF(location, ''), ?),
		    posted_at = COALESCE(NULLIF(posted_at, ''), ?),
		    scraped_at = COALESCE(NULLIF(scraped_at, ''), ?),
		    description = COALESCE(NULLIF(description, ''), ?),
		    updated_at = datetime('now')
		WHERE id = ? AND user_id = ?`,
		title, company, url, platform, location, postedAt, scrapedAt, description, canonID, r.userID,
	)
	if err != nil {
		return fmt.Errorf("backfill canonical job: %w", err)
	}
	return nil
}

// mergeAlternateState folds the alternate's pipeline state into the canonical
// row: status is escalated by rank, first_seen_at takes the earlier date, and
// other state columns are backfilled only where the canonical value is blank.
// Ported from mergeAlternateState in lib/db.js.
func (r *Repository) mergeAlternateState(ex sqlExec, alt *Job, canonicalID string) error {
	canonical, ok, err := r.getJobByID(ex, canonicalID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	status := preferredStatus(strOrEmpty(canonical.Status), strOrEmpty(alt.Status))
	firstSeen := earliestDate(firstSeenInherit(canonical), firstSeenInherit(alt))

	_, err = ex.Exec(`
		UPDATE jobs
		SET score = ?, reasoning = ?, outreach = ?, status = ?, first_seen_at = ?,
		    applied_at = ?, stage = ?, notes = ?, reached_out_at = ?, interview_notes = ?,
		    apply_complexity = ?, rejected_from_stage = ?, rejected_at = ?, claude_score = ?,
		    claude_reasoning = ?, rejection_reasoning = ?, score_attempts = ?,
		    last_score_attempt_at = ?, score_error = ?, updated_at = datetime('now')
		WHERE id = ? AND user_id = ?`,
		mergeInt(canonical.Score, alt.Score),
		mergeStr(canonical.Reasoning, alt.Reasoning),
		mergeStr(canonical.Outreach, alt.Outreach),
		status,
		nullable(firstSeen),
		mergeStr(canonical.AppliedAt, alt.AppliedAt),
		mergeStr(canonical.Stage, alt.Stage),
		mergeStr(canonical.Notes, alt.Notes),
		mergeStr(canonical.ReachedOutAt, alt.ReachedOutAt),
		mergeStr(canonical.InterviewNotes, alt.InterviewNotes),
		mergeStr(canonical.ApplyComplexity, alt.ApplyComplexity),
		mergeStr(canonical.RejectedFromStage, alt.RejectedFromStage),
		mergeStr(canonical.RejectedAt, alt.RejectedAt),
		mergeInt(canonical.ClaudeScore, alt.ClaudeScore),
		mergeStr(canonical.ClaudeReasoning, alt.ClaudeReasoning),
		mergeStr(canonical.RejectionReasoning, alt.RejectionReasoning),
		mergeInt(canonical.ScoreAttempts, alt.ScoreAttempts),
		mergeStr(canonical.LastScoreAttemptAt, alt.LastScoreAttemptAt),
		mergeStr(canonical.ScoreError, alt.ScoreError),
		canonicalID, r.userID,
	)
	if err != nil {
		return fmt.Errorf("merge alternate state: %w", err)
	}
	return nil
}

// rekeyDependentRows moves audit/event rows from the alternate id to the
// canonical id. Ported from rekeyDependentRows in lib/db.js (artifact migrations
// were retired, so it returns no moves).
func (r *Repository) rekeyDependentRows(ex sqlExec, alternateID, canonicalID string) error {
	if _, err := ex.Exec("UPDATE events SET job_id = ? WHERE job_id = ? AND user_id = ?", canonicalID, alternateID, r.userID); err != nil {
		return fmt.Errorf("rekey events: %w", err)
	}
	if _, err := ex.Exec("UPDATE rejection_email_log SET matched_job_id = ? WHERE matched_job_id = ? AND user_id = ?", canonicalID, alternateID, r.userID); err != nil {
		return fmt.Errorf("rekey rejection_email_log: %w", err)
	}
	return nil
}

// recordJobAlias upserts the alternate→canonical mapping into job_aliases.
// Ported from recordJobAlias in lib/db.js (extraEvidence is always empty now).
func (r *Repository) recordJobAlias(ex sqlExec, alt *Job, res Resolution, status, canonRowID string) error {
	evidence := res.Evidence
	if evidence == nil {
		evidence = map[string]any{}
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("marshal alias evidence: %w", err)
	}

	// canonical_job_id must point at the actual jobs.id of the canonical row. The
	// caller passes the tenant-local row id (canonRowID) for a primary resolution;
	// when empty (non-primary), fall back to the resolved id.
	var canonicalID any
	switch {
	case canonRowID != "":
		canonicalID = canonRowID
	case res.Job != nil && res.Job.ID != "":
		canonicalID = res.Job.ID
	}
	var confidence any
	if res.Confidence != 0 {
		confidence = res.Confidence
	}

	_, err = ex.Exec(`
		INSERT INTO job_aliases (
			user_id, alternate_job_id, canonical_job_id, original_platform, original_url,
			resolved_platform, resolved_url, status, confidence, evidence_json, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(alternate_job_id) DO UPDATE SET
			canonical_job_id = excluded.canonical_job_id,
			original_platform = excluded.original_platform,
			original_url = excluded.original_url,
			resolved_platform = excluded.resolved_platform,
			resolved_url = excluded.resolved_url,
			status = excluded.status,
			confidence = excluded.confidence,
			evidence_json = excluded.evidence_json,
			updated_at = datetime('now')`,
		r.userID,
		alt.ID,
		canonicalID,
		nullStr(alt.Platform),
		nullStr(alt.URL),
		nullable(res.Platform),
		nullable(res.URL),
		status,
		confidence,
		string(evidenceJSON),
	)
	if err != nil {
		return fmt.Errorf("record job alias: %w", err)
	}
	return nil
}

// archiveAlternateJob marks the alternate row archived, recording why (folded
// into a canonical primary vs. an unsupported URL). Ported from
// archiveAlternateJob in lib/db.js.
func (r *Repository) archiveAlternateJob(ex sqlExec, alternateID, reason string) error {
	_, err := ex.Exec(
		"UPDATE jobs SET status = 'archived', archive_reason = ?, updated_at = datetime('now') WHERE id = ? AND user_id = ?",
		reason, alternateID, r.userID,
	)
	if err != nil {
		return fmt.Errorf("archive alternate job: %w", err)
	}
	return nil
}

const jobColumns = `id, title, company, url, platform, location, posted_at, scraped_at,
	description, created_at, first_seen_at, score, reasoning, outreach, status,
	applied_at, stage, notes, reached_out_at, interview_notes, apply_complexity,
	rejected_from_stage, rejected_at, claude_score, claude_reasoning,
	rejection_reasoning, score_attempts, last_score_attempt_at, score_error`

// getJobByID loads a full jobs row. The boolean is false when no row matches.
// Ported from getJobById in lib/db.js.
func (r *Repository) getJobByID(ex sqlExec, id string) (*Job, bool, error) {
	j, err := scanJob(ex.QueryRow("SELECT "+jobColumns+" FROM jobs WHERE id = ? AND user_id = ?", id, r.userID))
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return j, true, nil
}

// SelectAlternateJobs returns non-primary ATS rows for canonicalization. When
// onlyPending is true, it matches scripts/resolve-ats-aliases.js --only-pending.
func (r *Repository) SelectAlternateJobs(onlyPending bool) ([]*Job, error) {
	where := "WHERE 1 = 1"
	if onlyPending {
		where = "WHERE status = 'pending'"
	}
	rows, err := r.query(`
		SELECT `+jobColumns+`
		FROM jobs
		`+where+`
			AND LOWER(COALESCE(platform, '')) NOT IN ('ashby', 'greenhouse', 'lever', 'workday')
			AND user_id = ?
		ORDER BY
			CASE status WHEN 'pending' THEN 0 WHEN 'applied' THEN 1 ELSE 2 END,
			platform,
			company,
			title`, r.userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

type jobScanner interface {
	Scan(dest ...any) error
}

func scanJob(scanner jobScanner) (*Job, error) {
	var j Job
	err := scanner.Scan(
		&j.ID, &j.Title, &j.Company, &j.URL, &j.Platform, &j.Location, &j.PostedAt,
		&j.ScrapedAt, &j.Description, &j.CreatedAt, &j.FirstSeenAt, &j.Score, &j.Reasoning,
		&j.Outreach, &j.Status, &j.AppliedAt, &j.Stage, &j.Notes, &j.ReachedOutAt,
		&j.InterviewNotes, &j.ApplyComplexity, &j.RejectedFromStage, &j.RejectedAt,
		&j.ClaudeScore, &j.ClaudeReasoning, &j.RejectionReasoning, &j.ScoreAttempts,
		&j.LastScoreAttemptAt, &j.ScoreError,
	)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// ---------------------------------------------------------------------------
// Pure helpers ported from lib/db.js (blank, preferredStatus, earliestDate, etc.)
// ---------------------------------------------------------------------------

// preferredStatus picks the status to keep when merging, ported verbatim from
// preferredStatus in lib/db.js: higher-ranked statuses win, except 'archived'
// never overwrites a non-archived status.
func preferredStatus(currentStatus, alternateStatus string) string {
	rank := map[string]int{
		"pending": 1, "archived": 2, "closed": 3,
		"rejected": 4, "responded": 5, "applied": 6,
	}
	current := currentStatus
	if current == "" {
		current = "pending"
	}
	alternate := alternateStatus
	if alternate == "" {
		alternate = "pending"
	}
	if alternate == "archived" && current != "archived" {
		return current
	}
	ra := rank[alternate]
	if ra == 0 {
		ra = 1
	}
	rc := rank[current]
	if rc == 0 {
		rc = 1
	}
	if ra > rc {
		return alternate
	}
	return current
}

// earliestDate returns the earlier of two timestamps, ported from earliestDate
// in lib/db.js. DB timestamps ('YYYY-MM-DD HH:MM:SS' / ISO) sort lexicographically
// in chronological order, so a string comparison matches the JS Date.parse result.
func earliestDate(left, right string) string {
	if blank(left) {
		return right
	}
	if blank(right) {
		return left
	}
	if right < left {
		return right
	}
	return left
}

// blank mirrors blank() in lib/db.js: null or empty string.
func blank(s string) bool { return s == "" }

// firstSeenInherit returns first_seen_at, falling back to created_at, mirroring
// `inherited.first_seen_at || inherited.created_at` in lib/db.js.
func firstSeenInherit(j *Job) string {
	return firstNonEmpty(strOrEmpty(j.FirstSeenAt), strOrEmpty(j.CreatedAt))
}

// mergeStr resolves one TEXT state column: backfill from the alternate only when
// the canonical value is blank; otherwise keep the canonical value (NULL stays
// NULL). Mirrors the generic branch of mergeAlternateState.
func mergeStr(canonical, alternate sql.NullString) any {
	if blank(strOrEmpty(canonical)) && !blank(strOrEmpty(alternate)) {
		return alternate.String
	}
	if canonical.Valid {
		return canonical.String
	}
	return nil
}

// mergeInt is mergeStr for the numeric state columns (NULL == blank; 0 is kept).
func mergeInt(canonical, alternate sql.NullInt64) any {
	if !canonical.Valid && alternate.Valid {
		return alternate.Int64
	}
	if canonical.Valid {
		return canonical.Int64
	}
	return nil
}

// strOrEmpty unwraps a NullString to "" when NULL.
func strOrEmpty(s sql.NullString) string {
	if s.Valid {
		return s.String
	}
	return ""
}

// nullStr returns the string, or nil when blank (mirrors `value || null`).
func nullStr(s sql.NullString) any {
	if s.Valid && s.String != "" {
		return s.String
	}
	return nil
}

// intOrNil returns the int, or nil when NULL (mirrors `value ?? null`; 0 is kept).
func intOrNil(n sql.NullInt64) any {
	if n.Valid {
		return n.Int64
	}
	return nil
}

// firstNonEmpty returns the first non-empty argument, or "".
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// joinCols joins column names with ", " for SQL composition.
func joinCols(cols []string) string {
	out := ""
	for i, c := range cols {
		if i > 0 {
			out += ", "
		}
		out += c
	}
	return out
}
