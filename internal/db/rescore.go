package db

import (
	"database/sql"
	"strings"
)

// yearsShortfallSQL matches a reasoning string whose stated mismatch is a
// years-of-experience shortfall. Written with LOWER(...) LIKE rather than a
// regex so it means the same thing on both dialects (PG's LIKE is
// case-sensitive; SQLite's is not). It is a heuristic over free text, so it
// over-matches by design: a re-score is cheap and simply confirms the rows that
// were rejected for a real reason.
const yearsShortfallSQL = `LOWER(COALESCE(reasoning, '')) LIKE '%year%' AND (` +
	`LOWER(COALESCE(reasoning, '')) LIKE '%failing to meet%' OR ` +
	`LOWER(COALESCE(reasoning, '')) LIKE '%fails to meet%' OR ` +
	`LOWER(COALESCE(reasoning, '')) LIKE '%does not meet%' OR ` +
	`LOWER(COALESCE(reasoning, '')) LIKE '%falls short%' OR ` +
	`LOWER(COALESCE(reasoning, '')) LIKE '%short of%' OR ` +
	`LOWER(COALESCE(reasoning, '')) LIKE '%below the minimum%')`

// JobsToRescore selects already-scored rows for a re-score. It deliberately does
// NOT filter on score IS NULL the way GetUnscoredJobs does: a re-score must
// never null a score to make a row eligible, because a pending row with a NULL
// score is exactly what ArchiveStaleUnscored sweeps (it keys on posted_at /
// first_seen_at, which no amount of touching updated_at can refresh).
//
// ids selects an explicit set. When ids is empty, yearsOnly restricts to rows a
// years shortfall buried at or below maxScore. Rows with no description are
// excluded: neither scoring path can evaluate one. limit <= 0 means no limit.
func (r *Repository) JobsToRescore(ids []string, yearsOnly bool, maxScore, limit int) ([]UnscoredJob, error) {
	q := `
		SELECT id, title, company, location, description
		FROM jobs
		WHERE user_id = ?
		  AND TRIM(COALESCE(description, '')) <> ''`
	args := []any{r.userID}

	switch {
	case len(ids) > 0:
		q += `
		  AND id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	case yearsOnly:
		q += `
		  AND score IS NOT NULL AND score <= ?
		  AND (` + yearsShortfallSQL + `)`
		args = append(args, maxScore)
	default:
		return nil, nil
	}

	q += `
		ORDER BY COALESCE(CASE WHEN substr(posted_at, 1, 2) = '20' THEN posted_at END, created_at) DESC`
	if limit > 0 {
		q += `
		LIMIT ?`
		args = append(args, limit)
	}

	rows, err := r.query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []UnscoredJob
	for rows.Next() {
		var j UnscoredJob
		var title, company, location, description sql.NullString
		if err := rows.Scan(&j.ID, &title, &company, &location, &description); err != nil {
			return nil, err
		}
		j.Title, j.Company, j.Location, j.Description = title.String, company.String, location.String, description.String
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// UnarchiveAboveThreshold returns auto-archived rows whose fresh score cleared
// the archive threshold to pending. Scoped to the ids just re-scored and to
// automatic archive reasons, so a row the user archived by hand (NULL/empty
// archive_reason, per archivereason.go) stays archived: a re-score may correct
// the model, never the user. Reports how many rows came back.
func (r *Repository) UnarchiveAboveThreshold(ids []string, threshold int) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	q := `
		UPDATE jobs
		SET status = 'pending', archive_reason = NULL, updated_at = datetime('now')
		WHERE user_id = ? AND status = 'archived'
		  AND score IS NOT NULL AND score > ?
		  AND archive_reason IN (?, ?)
		  AND id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
	args := []any{r.userID, threshold, ArchiveReasonLowScore, ArchiveReasonStaleUnscored}
	for _, id := range ids {
		args = append(args, id)
	}
	res, err := r.exec(q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
