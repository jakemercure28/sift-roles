package db

import (
	"database/sql"
	"time"
)

// AnalyticsJob is a jobs row as the analytics layer reads it.
type AnalyticsJob struct {
	ID        string
	Title     string
	Company   string
	URL       string
	Location  string
	Score     *int
	Status    string
	Stage     string
	AppliedAt *string
	PostedAt  string
	CreatedAt string
}

// AnalyticsEvent is an events row.
type AnalyticsEvent struct {
	JobID     string
	EventType string
	FromValue string
	ToValue   string
	CreatedAt string
}

// AnalyticsJobs returns all jobs in insertion (rowid) order, matching the
// unordered SELECT in computeAnalyticsMetrics.
func (r *Repository) AnalyticsJobs() ([]AnalyticsJob, error) {
	rows, err := r.query(`SELECT id, title, company, url, location, score, status, stage, applied_at, posted_at, created_at FROM jobs WHERE user_id = ?`, r.userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AnalyticsJob
	for rows.Next() {
		var j AnalyticsJob
		var title, company, url, location, status, stage, postedAt, createdAt sql.NullString
		var appliedAt sql.NullString
		var score sql.NullInt64
		if err := rows.Scan(&j.ID, &title, &company, &url, &location, &score, &status, &stage, &appliedAt, &postedAt, &createdAt); err != nil {
			return nil, err
		}
		j.Title, j.Company, j.URL, j.Location = title.String, company.String, url.String, location.String
		j.Status, j.Stage, j.PostedAt, j.CreatedAt = status.String, stage.String, postedAt.String, createdAt.String
		if score.Valid {
			v := int(score.Int64)
			j.Score = &v
		}
		if appliedAt.Valid {
			s := appliedAt.String
			j.AppliedAt = &s
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// AnalyticsEvents returns all events in insertion order.
func (r *Repository) AnalyticsEvents() ([]AnalyticsEvent, error) {
	rows, err := r.query(`SELECT job_id, event_type, from_value, to_value, created_at FROM events WHERE user_id = ?`, r.userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AnalyticsEvent
	for rows.Next() {
		var e AnalyticsEvent
		var from, to, created sql.NullString
		if err := rows.Scan(&e.JobID, &e.EventType, &from, &to, &created); err != nil {
			return nil, err
		}
		e.FromValue, e.ToValue, e.CreatedAt = from.String, to.String, created.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// AnalyticsStageEvents returns only pipeline stage-change events. The Analytics
// page needs this smaller set for applied/rejected evidence; the full audit path
// keeps using AnalyticsEvents so its JSON parity stays unchanged.
func (r *Repository) AnalyticsStageEvents() ([]AnalyticsEvent, error) {
	rows, err := r.query(`SELECT job_id, event_type, from_value, to_value, created_at FROM events WHERE event_type = 'stage_change' AND user_id = ?`, r.userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AnalyticsEvent
	for rows.Next() {
		var e AnalyticsEvent
		var from, to, created sql.NullString
		if err := rows.Scan(&e.JobID, &e.EventType, &from, &to, &created); err != nil {
			return nil, err
		}
		e.FromValue, e.ToValue, e.CreatedAt = from.String, to.String, created.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// trackedStages mirrors TRACKED_STAGES in stage-stats.js.
var trackedStages = []any{"phone_screen", "interview", "onsite", "offer"}

// reachedCTE counts a job as having reached a tracked stage only when it
// genuinely stood there, so brief test-flips (e.g. flipping a job to "interview"
// for a few minutes while testing the dashboard) no longer inflate the funnel.
// A job qualifies via any of four branches:
//
//  1. A recorded stage episode that lasted at least one day. Each stage_change
//     event marks entry into e.to_value; the job stays there until its next
//     event (LEAD). A closed episode counts when next-entered >= 1 day; an open
//     episode (still in that stage) counts when it was entered on or before the
//     cutoff (now - 1 day). Sub-day episodes drop out — that is the flip filter.
//  2. A pre-history stage: the job *left* a tracked stage (from_value) that we
//     never saw it enter (no matching to_value), so its dwell predates recorded
//     history. It genuinely reached the stage; dwell is unmeasurable, so it is
//     counted. The NOT EXISTS guard means real flips (which always have a
//     to_value entry) fall to branch 1's dwell test instead of counting here.
//  3. It currently sits in a tracked stage and has done so since the cutoff.
//  4. It was rejected out of a tracked stage (rejected_from_stage) — a rejection
//     from a stage is proof it truly reached it.
//
// The cutoff (?) is bound twice (branches 1 and 3); see reachedCTEArgs.
const reachedCTE = `
	WITH reached AS (
		SELECT ep.job_id, ep.stage FROM (
			SELECT e.job_id AS job_id, e.to_value AS stage, e.created_at AS entered,
				LEAD(e.created_at) OVER (PARTITION BY e.user_id, e.job_id ORDER BY e.created_at, e.id) AS next_at
			FROM events e JOIN jobs j ON j.id = e.job_id AND j.user_id = e.user_id
			WHERE e.event_type = 'stage_change' AND e.user_id = ?
		) ep
		WHERE ep.stage IN (?,?,?,?)
			AND (
				(ep.next_at IS NULL AND ep.entered <= ?)
				OR (ep.next_at IS NOT NULL AND (julianday(ep.next_at) - julianday(ep.entered)) >= 1)
			)
		UNION
		SELECT e.job_id, e.from_value AS stage
		FROM events e JOIN jobs j ON j.id = e.job_id AND j.user_id = e.user_id
		WHERE e.event_type = 'stage_change' AND e.from_value IN (?,?,?,?) AND e.user_id = ?
			AND NOT EXISTS (
				SELECT 1 FROM events t
				WHERE t.job_id = e.job_id AND t.user_id = e.user_id
					AND t.event_type = 'stage_change' AND t.to_value = e.from_value
			)
		UNION
		SELECT id AS job_id, stage FROM jobs
		WHERE stage IN (?,?,?,?) AND user_id = ? AND updated_at <= ?
		UNION
		SELECT id AS job_id, rejected_from_stage AS stage FROM jobs
		WHERE rejected_from_stage IN (?,?,?,?) AND user_id = ?
	)`

// reachedCTEArgs builds the 22 bind args for reachedCTE in placeholder order:
// branch 1 (user id, 4 stages, cutoff), branch 2 (4 stages, user id), branch 3
// (4 stages, user id, cutoff), branch 4 (4 stages, user id). cutoff is the
// timestamp (now - 1 day) gating the dwell branches; see reachedCTE.
func (r *Repository) reachedCTEArgs(cutoff string) []any {
	args := make([]any, 0, 22)
	// branch 1: episodes
	args = append(args, r.userID)
	args = append(args, trackedStages...)
	args = append(args, cutoff)
	// branch 2: pre-history from_value
	args = append(args, trackedStages...)
	args = append(args, r.userID)
	// branch 3: current stage past cutoff
	args = append(args, trackedStages...)
	args = append(args, r.userID, cutoff)
	// branch 4: rejected_from_stage
	args = append(args, trackedStages...)
	args = append(args, r.userID)
	return args
}

// ReachedCutoff renders the dwell cutoff (now - reachedDwell) in the stored
// 'YYYY-MM-DD HH:MM:SS' UTC text format used for timestamp columns on both
// backends. Callers pass the analytics "now" so the reached counts are
// deterministic rather than wall-clock dependent.
func ReachedCutoff(now time.Time) string {
	return now.UTC().Add(-reachedDwell).Format("2006-01-02 15:04:05")
}

// reachedDwell is the minimum time a job must stand at a stage for it to count
// as reached. "At least a day" filters brief test-flips.
const reachedDwell = 24 * time.Hour

// ReachedRow is one (job_id, stage) reached-stage pair.
type ReachedRow struct {
	JobID string
	Stage string
}

// ReachedStageRows ports getReachedRows in stage-stats.js. cutoff is now - 1 day
// (see ReachedCutoff); it gates the dwell branches of reachedCTE.
func (r *Repository) ReachedStageRows(cutoff string) ([]ReachedRow, error) {
	rows, err := r.query(reachedCTE+" SELECT job_id, stage FROM reached", r.reachedCTEArgs(cutoff)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReachedRow
	for rows.Next() {
		var rr ReachedRow
		if err := rows.Scan(&rr.JobID, &rr.Stage); err != nil {
			return nil, err
		}
		out = append(out, rr)
	}
	return out, rows.Err()
}

// AdvancedCountsByScore ports getHistoricalAdvancedCountsByScore: score -> count
// of distinct advanced applied jobs at that score.
func (r *Repository) AdvancedCountsByScore(cutoff string) (map[int]int, error) {
	args := append(r.reachedCTEArgs(cutoff), r.userID)
	rows, err := r.query(reachedCTE+`
		SELECT j.score, COUNT(DISTINCT j.id) AS advanced
		FROM jobs j JOIN reached rc ON rc.job_id = j.id
		WHERE j.score IS NOT NULL AND j.applied_at IS NOT NULL AND j.user_id = ?
		GROUP BY j.score`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]int{}
	for rows.Next() {
		var score, advanced int
		if err := rows.Scan(&score, &advanced); err != nil {
			return nil, err
		}
		out[score] = advanced
	}
	return out, rows.Err()
}

// RecentEvent is a row of the event feed (events joined to jobs).
type RecentEvent struct {
	EventType string
	FromValue *string
	ToValue   *string
	CreatedAt string
	Company   string
	Title     string
}

// RecentEvents returns the human-facing event feed newest-first. Scrape and ATS
// resolution events stay available to backend logs/analytics, but are internal
// intake noise for the dashboard Activity Log.
func (r *Repository) RecentEvents() ([]RecentEvent, error) {
	rows, err := r.query(`
		SELECT e.event_type, e.from_value, e.to_value, e.created_at, j.company, j.title
		FROM events e JOIN jobs j ON e.job_id = j.id
		WHERE e.user_id = ?
		  AND e.event_type IN ('stage_change', 'status_change', 'outreach', 'auto_applied')
		ORDER BY e.created_at DESC`, r.userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecentEvent
	for rows.Next() {
		var e RecentEvent
		var from, to, company, title, created sql.NullString
		if err := rows.Scan(&e.EventType, &from, &to, &created, &company, &title); err != nil {
			return nil, err
		}
		e.CreatedAt, e.Company, e.Title = created.String, company.String, title.String
		if from.Valid {
			s := from.String
			e.FromValue = &s
		}
		if to.Valid {
			s := to.String
			e.ToValue = &s
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RejectionInsight is one rejection row with timing deltas.
type RejectionInsight struct {
	RejectedFrom *string
	Company      string
	Title        string
	Score        *int
	PostedAt     string
	AppliedAt    string
	DaysToReject *float64
	PostingAge   *float64
}

// RejectionInsights mirrors the rejectionInsights query in fetchAnalyticsData.
func (r *Repository) RejectionInsights() ([]RejectionInsight, error) {
	rows, err := r.query(`
		SELECT e.from_value AS rejected_from, j.company, j.title, j.score, j.posted_at, j.applied_at,
			ROUND(julianday(e.created_at) - julianday(j.applied_at), 1) AS days_to_reject,
			ROUND(julianday(j.applied_at) - julianday(j.posted_at), 1) AS posting_age
		FROM events e JOIN jobs j ON e.job_id = j.id
		WHERE e.to_value = 'rejected' AND j.status != 'pending' AND e.user_id = ?
		ORDER BY e.created_at DESC`, r.userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RejectionInsight
	for rows.Next() {
		var ri RejectionInsight
		var from, company, title, posted, applied sql.NullString
		var score sql.NullInt64
		var dtr, age sql.NullFloat64
		if err := rows.Scan(&from, &company, &title, &score, &posted, &applied, &dtr, &age); err != nil {
			return nil, err
		}
		ri.Company, ri.Title, ri.PostedAt, ri.AppliedAt = company.String, title.String, posted.String, applied.String
		if from.Valid {
			s := from.String
			ri.RejectedFrom = &s
		}
		if score.Valid {
			v := int(score.Int64)
			ri.Score = &v
		}
		if dtr.Valid {
			ri.DaysToReject = &dtr.Float64
		}
		if age.Valid {
			ri.PostingAge = &age.Float64
		}
		out = append(out, ri)
	}
	return out, rows.Err()
}
