package db

import "fmt"

// DedupResult counts the rows archived by each dedup pass.
type DedupResult struct {
	Reposts    int64
	Pending    int64
	Alternates int64
}

// Total is the number of rows archived across all three passes.
func (d DedupResult) Total() int64 { return d.Reposts + d.Pending + d.Alternates }

// The three dedup passes, ported verbatim from dedupExistingJobs
// (lib/pipeline/import.js). They operate on the whole jobs table (not a scraped
// batch), so behavior is identical whether rows were inserted by the old Node
// pipeline or by the Go engine. Idempotent.
const (
	// Archive re-posts where the older version was genuinely reviewed and then
	// dismissed (scored, archived, no stage). The j2.score IS NOT NULL guard is
	// load-bearing: the pending and alternates passes below archive *unscored*
	// duplicate copies, so without it a deduped copy would make this pass treat
	// every future re-scrape of the same listing as a "dismissed repost" and
	// archive it before it can ever be scored, cascading a unique job to zero
	// surviving rows.
	dedupRepostsSQL = `
UPDATE jobs SET status = 'archived', archive_reason = '` + ArchiveReasonDedupRepost + `', updated_at = datetime('now')
WHERE status = 'pending' AND score IS NULL AND jobs.user_id = ?
  AND EXISTS (
    SELECT 1 FROM jobs j2
    WHERE LOWER(TRIM(j2.title)) = LOWER(TRIM(jobs.title))
      AND LOWER(TRIM(j2.company)) = LOWER(TRIM(jobs.company))
      AND j2.id != jobs.id
      AND j2.user_id = jobs.user_id
      AND j2.status = 'archived'
      AND j2.score IS NOT NULL
      AND (j2.stage IS NULL OR j2.stage = '')
  )`

	// For pending-only duplicates (same title+company, none archived), keep one
	// survivor. Prefer a primary ATS row over an aggregator copy so the kept row
	// is the canonical one (and the alternates pass below then has nothing to do);
	// break ties by newest. Keeping the aggregator instead would leave the
	// alternates pass to archive it next, cascading to zero survivors.
	dedupPendingSQL = `
UPDATE jobs SET status = 'archived', archive_reason = '` + ArchiveReasonDedupPending + `', updated_at = datetime('now')
WHERE status = 'pending' AND score IS NULL AND jobs.user_id = ?
  AND id NOT IN (
    SELECT id FROM (
      SELECT id, ROW_NUMBER() OVER (
        PARTITION BY LOWER(TRIM(title)), LOWER(TRIM(company))
        ORDER BY
          CASE WHEN LOWER(COALESCE(platform, '')) IN ('ashby', 'greenhouse', 'lever', 'workday') THEN 0 ELSE 1 END,
          created_at DESC
      ) as rn
      FROM jobs WHERE status = 'pending' AND user_id = ?
    ) WHERE rn = 1
  )
  AND EXISTS (
    SELECT 1 FROM jobs j2
    WHERE LOWER(TRIM(j2.title)) = LOWER(TRIM(jobs.title))
      AND LOWER(TRIM(j2.company)) = LOWER(TRIM(jobs.company))
      AND j2.id != jobs.id
      AND j2.user_id = jobs.user_id
      AND j2.status = 'pending'
  )`

	// Archive alternate-source jobs that duplicate a primary ATS row. The source
	// must still be a live row (not archived): an archived ATS source may itself
	// be a dedup/low-score casualty, and archiving the aggregator copy against it
	// would leave the listing with zero surviving rows.
	dedupAlternatesSQL = `
UPDATE jobs SET status = 'archived', archive_reason = '` + ArchiveReasonDedupAlternate + `', updated_at = datetime('now')
WHERE status = 'pending' AND LOWER(COALESCE(platform, '')) NOT IN ('ashby', 'greenhouse', 'lever', 'workday') AND jobs.user_id = ?
  AND EXISTS (
    SELECT 1 FROM jobs source
    WHERE LOWER(COALESCE(source.platform, '')) IN ('ashby', 'greenhouse', 'lever', 'workday')
      AND LOWER(TRIM(source.title)) = LOWER(TRIM(jobs.title))
      AND LOWER(TRIM(source.company)) = LOWER(TRIM(jobs.company))
      AND source.id != jobs.id
      AND source.user_id = jobs.user_id
      AND source.status != 'archived'
  )`
)

// DedupExistingJobs runs the three dedup passes in a single transaction and
// returns the per-pass archive counts.
func (r *Repository) DedupExistingJobs() (DedupResult, error) {
	tx, err := r.db.Begin()
	if err != nil {
		return DedupResult{}, err
	}
	defer tx.Rollback()
	ex := execer{raw: tx, dl: r.dl}

	var res DedupResult
	exec := func(query string, args ...any) (int64, error) {
		out, execErr := ex.Exec(query, args...)
		if execErr != nil {
			return 0, execErr
		}
		return out.RowsAffected()
	}

	if res.Reposts, err = exec(dedupRepostsSQL, r.userID); err != nil {
		return DedupResult{}, fmt.Errorf("dedup reposts: %w", err)
	}
	if res.Pending, err = exec(dedupPendingSQL, r.userID, r.userID); err != nil {
		return DedupResult{}, fmt.Errorf("dedup pending: %w", err)
	}
	if res.Alternates, err = exec(dedupAlternatesSQL, r.userID); err != nil {
		return DedupResult{}, fmt.Errorf("dedup alternates: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return DedupResult{}, err
	}
	return res, nil
}

// AutoGhostStale marks applied jobs with no progress for more than `days` as
// ghosted and logs a stage_change event for each, ported from
// scripts/auto-ghost.js. Returns the number ghosted.
func (r *Repository) AutoGhostStale(days int) (int, error) {
	rows, err := r.query(
		`SELECT id, COALESCE(stage, '') FROM jobs
		 WHERE status = 'applied'
		   AND COALESCE(stage, 'applied') = 'applied'
		   AND applied_at IS NOT NULL
		   AND applied_at < datetime('now', '-' || ? || ' days')
		   AND user_id = ?`,
		days, r.userID,
	)
	if err != nil {
		return 0, err
	}
	type stale struct{ id, stage string }
	var list []stale
	for rows.Next() {
		var s stale
		if err := rows.Scan(&s.id, &s.stage); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(list) == 0 {
		return 0, nil
	}

	tx, err := r.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	ex := execer{raw: tx, dl: r.dl}
	for _, s := range list {
		if _, err := ex.Exec(
			`UPDATE jobs SET status='ghosted', stage='ghosted', updated_at=datetime('now') WHERE id=? AND user_id=?`,
			s.id, r.userID,
		); err != nil {
			return 0, err
		}
		from := s.stage
		if from == "" {
			from = "applied" // matches `job.stage || 'applied'` in auto-ghost.js
		}
		if _, err := ex.Exec(
			`INSERT INTO events (user_id, job_id, event_type, from_value, to_value) VALUES (?, ?, 'stage_change', ?, 'ghosted')`,
			r.userID, s.id, from,
		); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(list), nil
}
