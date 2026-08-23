package pipeline

import (
	"context"
	"log/slog"

	"job-search-automation/internal/db"
)

// RescoreResult reports what a re-score pass did.
type RescoreResult struct {
	ScoreResult
	// Unarchived counts rows whose fresh score cleared the archive threshold
	// and came back to pending.
	Unarchived int64
}

// RescoreJobs re-scores an explicitly selected set of already-scored jobs and
// returns any that clear the archive threshold to pending.
//
// This exists because a prompt or rubric change makes existing scores stale,
// and the obvious recovery (null the score so GetUnscoredJobs picks the row up,
// then run score-once) is a trap: ArchiveStaleUnscored sweeps pending rows with
// a NULL score using posted_at / first_seen_at as the age key, so rows older
// than the stale window get re-buried before the scorer ever reaches them.
// Scoring in place never creates that window, because the old score stays on
// the row until the new one overwrites it.
//
// Jobs that score at or below the threshold are auto-archived by the shared
// scoring path exactly as a first-time score would be, so a re-score can bury a
// row as well as recover one.
func RescoreJobs(ctx context.Context, repo *db.Repository, sc JobScorer, jobs []db.UnscoredJob, cfg ScoreConfig, log *slog.Logger) (RescoreResult, error) {
	if len(jobs) == 0 {
		return RescoreResult{}, nil
	}

	res := scoreJobList(ctx, repo, sc, jobs, cfg, log, "mode", "rescore")

	ids := make([]string, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}
	n, err := repo.UnarchiveAboveThreshold(ids, cfg.ArchiveThreshold)
	if err != nil {
		// The scores are already written and correct; failing to unarchive is
		// recoverable by re-running, so report it without discarding the pass.
		log.Error("unarchive after rescore failed", "error", err)
		return RescoreResult{ScoreResult: res}, err
	}
	log.Info("rescore complete", "scored_ok", res.ScoredOK, "scored_failed", res.ScoredFailed, "unarchived", n)
	return RescoreResult{ScoreResult: res, Unarchived: n}, nil
}
