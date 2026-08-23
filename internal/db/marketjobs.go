package db

import (
	"database/sql"
	"fmt"
)

// This file ports lib/market-jobs.js: the job queries behind the Market Research
// view. liveMarketWhere keeps the "live" cohort (not in a terminal state).

const liveMarketWhere = `status NOT IN ('archived','closed','rejected','ghosted')
	AND COALESCE(stage, '') NOT IN ('archived','closed','rejected','ghosted')`

// marketDescTruncate caps how many characters of each description the Market
// Research queries pull over the pooler. The only consumers (classifySeniority,
// isAccessible, marketMatchesTerm, parseYearsFromDescription) scan the
// seniority/requirements block near the top of a JD, and the Gemini prompt
// hard-caps each JD at 600 chars, so 800 covers every reader while cutting the
// per-row payload ~3.7x versus the old 3000. This truncated text is the single
// largest Supabase egress source, so the cap lives in one place.
const marketDescTruncate = 800

// marketSeniorityCols is the shared SELECT list for the seniority/research job
// queries, with the description truncated to marketDescTruncate in SQL.
var marketSeniorityCols = fmt.Sprintf(
	`id, title, company, substr(description, 1, %d) AS description, score, status, applied_at, stage, rejected_from_stage, location, posted_at`,
	marketDescTruncate)

// MarketSeniorityJob is a jobs row as the seniority breakdown reads it.
type MarketSeniorityJob struct {
	ID                string
	Title             string
	Company           string
	Description       string
	Score             *int
	Status            string
	AppliedAt         *string
	Stage             string
	RejectedFromStage string
	Location          string
	PostedAt          string
}

func scanSeniorityJobs(rows Rows) ([]MarketSeniorityJob, error) {
	defer rows.Close()
	var out []MarketSeniorityJob
	for rows.Next() {
		var j MarketSeniorityJob
		var title, company, desc, status, stage, rejFrom, location, posted sql.NullString
		var applied sql.NullString
		var score sql.NullInt64
		if err := rows.Scan(&j.ID, &title, &company, &desc, &score, &status, &applied, &stage, &rejFrom, &location, &posted); err != nil {
			return nil, err
		}
		j.Title, j.Company, j.Description = title.String, company.String, desc.String
		j.Status, j.Stage, j.RejectedFromStage = status.String, stage.String, rejFrom.String
		j.Location, j.PostedAt = location.String, posted.String
		if score.Valid {
			v := int(score.Int64)
			j.Score = &v
		}
		if applied.Valid {
			s := applied.String
			j.AppliedAt = &s
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// LiveMarketSeniorityJobs ports getLiveMarketSeniorityJobs. The description is
// truncated to marketDescTruncate chars in SQL: the only consumers of the text are
// classifySeniority and isAccessible (regexes over the seniority/requirements
// section near the top of a JD), so shipping full multi-KB JDs over the remote
// pooler was wasted egress.
func (r *Repository) LiveMarketSeniorityJobs() ([]MarketSeniorityJob, error) {
	rows, err := r.query(`
		SELECT `+marketSeniorityCols+`
		FROM jobs WHERE `+liveMarketWhere+`
		AND user_id = ?
		ORDER BY score DESC, created_at DESC`, r.userID)
	if err != nil {
		return nil, err
	}
	return scanSeniorityJobs(rows)
}

// LiveMarketResearchJobs ports getLiveMarketResearchJobs: live rows with a
// substantial description, used as the Gemini/audit JD sample. The description is
// truncated to marketDescTruncate chars in SQL: the Gemini prompt hard-caps each JD at 600 chars
// (buildMarketResearchPrompt) and the audit's marketMatchesTerm scans the same
// sample, so the full multi-KB JD text never reached either consumer and was pure
// egress over the remote pooler. The length(description) > 100 filter still applies
// to the untruncated column, so the cohort membership is unchanged.
func (r *Repository) LiveMarketResearchJobs() ([]MarketSeniorityJob, error) {
	rows, err := r.query(`
		SELECT `+marketSeniorityCols+`
		FROM jobs WHERE `+liveMarketWhere+`
		AND description IS NOT NULL AND length(description) > 100
		AND user_id = ?
		ORDER BY score DESC, created_at DESC`, r.userID)
	if err != nil {
		return nil, err
	}
	return scanSeniorityJobs(rows)
}

// AllTimeMarketSeniorityJobs ports getAllTimeMarketSeniorityJobs. The description
// is truncated in SQL (marketDescTruncate): the only consumer of the text is
// parseYearsFromDescription (a regex over the requirements section near the top of
// a JD). windowDays > 0 bounds the scan to rows created in the last windowDays so
// the read does not grow unbounded as the table accumulates years of listings and
// re-ship them on every rebuild; windowDays <= 0 keeps the full all-time scope.
// The seniority/YOE distribution stays representative on a rolling window.
func (r *Repository) AllTimeMarketSeniorityJobs(windowDays int) ([]MarketSeniorityJob, error) {
	where := `title IS NOT NULL AND TRIM(title) != '' AND user_id = ?`
	args := []any{r.userID}
	if windowDays > 0 {
		where += ` AND created_at >= datetime('now', '-' || ? || ' days')`
		args = append(args, windowDays)
	}
	rows, err := r.query(`
		SELECT `+marketSeniorityCols+`
		FROM jobs WHERE `+where+`
		ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	return scanSeniorityJobs(rows)
}

// CountLiveMarketResearchJobs ports countLiveMarketResearchJobs (live cohort with
// a substantial description).
func (r *Repository) CountLiveMarketResearchJobs() (int, error) {
	var n int
	err := r.queryRow(`
		SELECT COUNT(*) FROM jobs WHERE `+liveMarketWhere+`
		AND description IS NOT NULL AND length(description) > 100
		AND user_id = ?`, r.userID).Scan(&n)
	return n, err
}

// CountAllTimeMarketResearchJobs ports countAllTimeMarketResearchJobs. windowDays
// mirrors AllTimeMarketSeniorityJobs so the page's all-time count matches the
// windowed sample it summarizes; windowDays <= 0 keeps the full all-time scope.
func (r *Repository) CountAllTimeMarketResearchJobs(windowDays int) (int, error) {
	where := `description IS NOT NULL AND length(description) > 100 AND user_id = ?`
	args := []any{r.userID}
	if windowDays > 0 {
		where += ` AND created_at >= datetime('now', '-' || ? || ' days')`
		args = append(args, windowDays)
	}
	var n int
	err := r.queryRow(`SELECT COUNT(*) FROM jobs WHERE `+where, args...).Scan(&n)
	return n, err
}

// CountAllTimeAppliedJobs ports countAllTimeAppliedJobs.
func (r *Repository) CountAllTimeAppliedJobs() (int, error) {
	var n int
	err := r.queryRow(`SELECT COUNT(*) FROM jobs WHERE applied_at IS NOT NULL AND user_id = ?`, r.userID).Scan(&n)
	return n, err
}

// MarketDataSignature returns a cheap fingerprint of the jobs table used to
// invalidate the dashboard's in-memory Market Research cache. It changes
// whenever a row is added, removed, or touched (updated_at bumps on scrape,
// score, status change, or a manual analysis re-run). Reading it is trivial
// next to loading every description into memory.
func (r *Repository) MarketDataSignature() (int, string, error) {
	var count int
	var maxUpdated string
	err := r.queryRow(`SELECT COUNT(*), COALESCE(MAX(updated_at), '') FROM jobs WHERE user_id = ?`, r.userID).Scan(&count, &maxUpdated)
	return count, maxUpdated, err
}

// MarketResearchSignature fingerprints only the rows that can affect the Market
// Research page: live rows in the current cohort, titled rows in the all-time
// seniority cohort, and applied rows used by the applied-count summary. It avoids
// invalidating the expensive rendered report for unrelated rows that the page
// never reads.
//
// It keys on COUNT(*) + MAX(created_at), deliberately NOT MAX(updated_at). Keying on
// updated_at busted the cache on nearly every tick (re-scores, status flips, and
// maintenance touches all bump it) and forced a full re-read of every description,
// which was the dominant Supabase egress. created_at is immutable, so a new max means
// a genuinely new row landed in the cohort; the every-6h scrape supplies that and
// refreshes the report. The tradeoff: a manual status change (archive/ghost) does not
// move COUNT or MAX(created_at) here, so the live breakdown can lag until the next
// scrape adds a row or the 23h TTL lapses. That staleness on an analytics overview is
// an acceptable price for eliminating the re-read loop.
func (r *Repository) MarketResearchSignature() (int, string, error) {
	var count int
	var maxCreated string
	err := r.queryRow(`
		SELECT COUNT(*), COALESCE(MAX(created_at), '') FROM jobs
		WHERE user_id = ?
		AND (
			(`+liveMarketWhere+`)
			OR (title IS NOT NULL AND TRIM(title) != '')
			OR applied_at IS NOT NULL
		)`, r.userID).Scan(&count, &maxCreated)
	return count, maxCreated, err
}
