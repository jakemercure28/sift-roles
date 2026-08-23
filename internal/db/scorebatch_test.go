package db

import (
	"fmt"
	"testing"
	"time"
)

// A submitted batch's members must vanish from the sync unscored queue and
// reappear once the batch closes (any terminal state deletes membership).
func TestScoreBatchSyncExclusion(t *testing.T) {
	repo := newTestRepo(t)
	seedJob(t, repo, "in-batch", "A", "Co")
	seedJob(t, repo, "free", "B", "Co")

	if err := repo.CreateScoreBatch("batches/x", "model-batch", "hash-1", []string{"in-batch"}); err != nil {
		t.Fatalf("CreateScoreBatch: %v", err)
	}

	jobs, err := repo.GetUnscoredJobs(0)
	if err != nil {
		t.Fatalf("GetUnscoredJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "free" {
		t.Fatalf("open batch not excluded: %+v", jobs)
	}

	// CountUnscored must be unaffected by batch membership (dashboard truth).
	if n, err := repo.CountUnscored(); err != nil || n != 2 {
		t.Fatalf("CountUnscored = (%d, %v), want (2, nil)", n, err)
	}

	if err := repo.CloseScoreBatch("batches/x", ScoreBatchExpired, "expired upstream"); err != nil {
		t.Fatalf("CloseScoreBatch: %v", err)
	}
	jobs, err = repo.GetUnscoredJobs(0)
	if err != nil {
		t.Fatalf("GetUnscoredJobs after close: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expired batch's member not freed: %+v", jobs)
	}
}

// A batch row older than OpenBatchExclusionMax no longer hides its members:
// if the poll loop died before closing it (feature disabled or key removed
// mid-flight), the jobs must return to the sync pool on their own. Upstream
// batches expire ~48h after creation, so past the bound their results can
// never arrive.
func TestScoreBatchExclusionTimeBound(t *testing.T) {
	repo := newTestRepo(t)
	seedJob(t, repo, "stranded", "A", "Co")

	if err := repo.CreateScoreBatch("batches/dead", "model-batch", "hash-1", []string{"stranded"}); err != nil {
		t.Fatalf("CreateScoreBatch: %v", err)
	}
	if jobs, err := repo.GetUnscoredJobs(0); err != nil || len(jobs) != 0 {
		t.Fatalf("fresh open batch must exclude its member: (%+v, %v)", jobs, err)
	}

	// Backdate the batch past the exclusion bound, as if the poll loop died
	// three days ago without ever closing it.
	old := time.Now().UTC().Add(-OpenBatchExclusionMax - time.Hour).Format("2006-01-02 15:04:05")
	if _, err := repo.exec(`UPDATE score_batches SET created_at = ? WHERE batch_name = ?`, old, "batches/dead"); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	jobs, err := repo.GetUnscoredJobs(0)
	if err != nil {
		t.Fatalf("GetUnscoredJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "stranded" {
		t.Fatalf("stale open batch still hides its member: %+v", jobs)
	}
}

// A batch open past OpenBatchExclusionMax must stop counting against the
// open-batch submit gate (its results can never arrive), while the oldest-open
// age signal keeps seeing it: that unbounded growth is the alert condition.
func TestCountOpenScoreBatchesAgeBound(t *testing.T) {
	repo := newTestRepo(t)
	seedJob(t, repo, "wedged", "A", "Co")

	if created, err := repo.OldestOpenScoreBatchCreatedAt(); err != nil || created != "" {
		t.Fatalf("oldest with no batches = (%q, %v), want empty", created, err)
	}

	if err := repo.CreateScoreBatch("batches/wedged", "m", "h", []string{"wedged"}); err != nil {
		t.Fatal(err)
	}
	if n, err := repo.CountOpenScoreBatches(); err != nil || n != 1 {
		t.Fatalf("fresh open count = (%d, %v), want 1", n, err)
	}

	old := time.Now().UTC().Add(-OpenBatchExclusionMax - time.Hour).Format("2006-01-02 15:04:05")
	if _, err := repo.exec(`UPDATE score_batches SET created_at = ? WHERE batch_name = ?`, old, "batches/wedged"); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	if n, err := repo.CountOpenScoreBatches(); err != nil || n != 0 {
		t.Fatalf("stale open count = (%d, %v), want 0", n, err)
	}
	if created, err := repo.OldestOpenScoreBatchCreatedAt(); err != nil || created != old {
		t.Fatalf("oldest open = (%q, %v), want the stale row's %q", created, err, old)
	}

	if err := repo.CloseScoreBatch("batches/wedged", ScoreBatchExpired, "evicted"); err != nil {
		t.Fatal(err)
	}
	if created, err := repo.OldestOpenScoreBatchCreatedAt(); err != nil || created != "" {
		t.Fatalf("oldest after close = (%q, %v), want empty", created, err)
	}
}

// RecentScoreBatches keeps every open row regardless of age but windows
// terminal rows to the caller's cutoff; GetScoreBatchByName is the admin
// CLI's cross-batch lookup.
func TestRecentAndLookupScoreBatches(t *testing.T) {
	repo := newTestRepo(t)
	seedJob(t, repo, "r1", "A", "Co")
	seedJob(t, repo, "r2", "B", "Co")
	seedJob(t, repo, "r3", "C", "Co")

	for name, job := range map[string]string{
		"batches/old-open": "r1", "batches/old-done": "r2", "batches/new-done": "r3",
	} {
		if err := repo.CreateScoreBatch(name, "m", "h", []string{job}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	for _, name := range []string{"batches/old-done", "batches/new-done"} {
		if err := repo.CloseScoreBatch(name, ScoreBatchSucceeded, ""); err != nil {
			t.Fatalf("close %s: %v", name, err)
		}
	}
	old := time.Now().UTC().AddDate(0, 0, -10).Format("2006-01-02 15:04:05")
	for _, name := range []string{"batches/old-open", "batches/old-done"} {
		if _, err := repo.exec(`UPDATE score_batches SET created_at = ? WHERE batch_name = ?`, old, name); err != nil {
			t.Fatalf("backdate %s: %v", name, err)
		}
	}

	since := time.Now().UTC().AddDate(0, 0, -7).Format("2006-01-02 15:04:05")
	got, err := repo.RecentScoreBatches(since)
	if err != nil {
		t.Fatalf("RecentScoreBatches: %v", err)
	}
	// old-done is terminal AND outside the window: the only excluded row.
	// Newest first, so the fresh terminal row precedes the ancient open one.
	if len(got) != 2 || got[0].BatchName != "batches/new-done" || got[1].BatchName != "batches/old-open" {
		t.Fatalf("recent batches = %+v, want [new-done, old-open]", got)
	}

	b, found, err := repo.GetScoreBatchByName("batches/old-open")
	if err != nil || !found || b.State != ScoreBatchSubmitted || b.JobCount != 1 {
		t.Fatalf("lookup = (%+v, %v, %v), want the open row", b, found, err)
	}
	if _, found, err := repo.GetScoreBatchByName("batches/nope"); err != nil || found {
		t.Fatalf("missing lookup = (found=%v, %v), want not found, no error", found, err)
	}
}

func TestScoreBatchLedger(t *testing.T) {
	repo := newTestRepo(t)
	seedJob(t, repo, "j1", "A", "Co")
	seedJob(t, repo, "j2", "B", "Co")

	if err := repo.CreateScoreBatch("batches/a", "m", "h", []string{"j1", "j2"}); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := repo.CreateScoreBatch("", "m", "h", nil); err == nil {
		t.Fatal("empty batch must be rejected")
	}

	open, err := repo.OpenScoreBatches()
	if err != nil || len(open) != 1 {
		t.Fatalf("OpenScoreBatches = (%+v, %v), want 1 row", open, err)
	}
	b := open[0]
	if b.BatchName != "batches/a" || b.State != ScoreBatchSubmitted || b.JobCount != 2 || b.UserID == "" {
		t.Fatalf("batch row = %+v", b)
	}

	if n, err := repo.CountOpenScoreBatches(); err != nil || n != 1 {
		t.Fatalf("CountOpenScoreBatches = (%d, %v)", n, err)
	}
	if n, err := repo.SumBatchJobsSubmittedSince("2000-01-01 00:00:00"); err != nil || n != 2 {
		t.Fatalf("SumBatchJobsSubmittedSince = (%d, %v), want 2", n, err)
	}
	if n, err := repo.SumBatchJobsSubmittedSince("2999-01-01 00:00:00"); err != nil || n != 0 {
		t.Fatalf("future SumBatchJobsSubmittedSince = (%d, %v), want 0", n, err)
	}

	// Closing twice is race-safe: the second close is a no-op.
	if err := repo.CloseScoreBatch("batches/a", ScoreBatchSucceeded, ""); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := repo.CloseScoreBatch("batches/a", ScoreBatchFailed, "late"); err != nil {
		t.Fatalf("double close: %v", err)
	}
	open, err = repo.OpenScoreBatches()
	if err != nil || len(open) != 0 {
		t.Fatalf("open after close = (%+v, %v)", open, err)
	}
	// Terminal batches still count against the daily submit budget.
	if n, err := repo.SumBatchJobsSubmittedSince("2000-01-01 00:00:00"); err != nil || n != 2 {
		t.Fatalf("closed SumBatchJobsSubmittedSince = (%d, %v), want 2", n, err)
	}
}

// ApplyBatchScoreResults must, in one transaction: land scores only on
// still-pending, still-unscored rows (user actions and sync scores during the
// in-flight window always win), stamp per-item errors under the same guard,
// auto-archive at the threshold, close the batch and free membership, and add
// the aggregated ledger exactly once even when a racing poller re-applies.
func TestApplyBatchScoreResults(t *testing.T) {
	repo := newTestRepo(t)
	seedJob(t, repo, "clean", "A", "Co")
	seedJob(t, repo, "low", "B", "Co")
	seedJob(t, repo, "sync-scored", "C", "Co")
	seedJob(t, repo, "user-archived", "D", "Co")
	seedJob(t, repo, "errored", "E", "Co")

	ids := []string{"clean", "low", "sync-scored", "user-archived", "errored"}
	if err := repo.CreateScoreBatch("batches/apply", "m-batch", "hash-1", ids); err != nil {
		t.Fatal(err)
	}
	// Mid-flight, a sync score and a user archive land on two members.
	if err := repo.UpdateJobScore("sync-scored", 8, "sync got here first"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(`UPDATE jobs SET status='archived' WHERE id='user-archived'`); err != nil {
		t.Fatal(err)
	}

	writes := []BatchScoreWrite{
		{JobID: "clean", Score: 7, Reasoning: "batch reasoning"},
		{JobID: "low", Score: 2, Reasoning: "weak"},
		{JobID: "sync-scored", Score: 3, Reasoning: "stale"},
		{JobID: "user-archived", Score: 9, Reasoning: "stale"},
		{JobID: "errored", ErrMsg: "Batch item failed: boom"},
	}
	usage := BatchUsageTotals{Calls: 5, Tokens: APITokenUsage{Prompt: 500, CachedPrompt: 10, Candidates: 50, Total: 560}}
	applied, claimed, err := repo.ApplyBatchScoreResults("batches/apply", writes, "hash-1", 3, "m-batch", usage)
	if err != nil || !claimed || applied != 2 {
		t.Fatalf("apply = (%d, %v, %v), want (2, true, nil)", applied, claimed, err)
	}

	assertJob := func(id string, wantScore any, wantStatus string) {
		t.Helper()
		var score any
		var status string
		if err := repo.db.QueryRow(`SELECT score, status FROM jobs WHERE id=?`, id).Scan(&score, &status); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if fmt.Sprint(score) != fmt.Sprint(wantScore) || status != wantStatus {
			t.Fatalf("%s = score %v status %s, want %v %s", id, score, status, wantScore, wantStatus)
		}
	}
	assertJob("clean", 7, "pending")
	assertJob("low", 2, "archived") // at the threshold: auto-archived
	assertJob("sync-scored", 8, "pending")
	assertJob("user-archived", nil, "archived")
	assertJob("errored", nil, "pending")

	var hash string
	if err := repo.db.QueryRow(`SELECT resume_hash FROM jobs WHERE id='clean'`).Scan(&hash); err != nil || hash != "hash-1" {
		t.Fatalf("applied hash = (%q, %v), want submit-time hash", hash, err)
	}
	var se, attempt any
	if err := repo.db.QueryRow(`SELECT score_error, last_score_attempt_at FROM jobs WHERE id='errored'`).Scan(&se, &attempt); err != nil {
		t.Fatal(err)
	}
	if se == nil || attempt == nil {
		t.Fatalf("errored row = (error %v, attempt %v), want both stamped", se, attempt)
	}
	if err := repo.db.QueryRow(`SELECT score_error FROM jobs WHERE id='sync-scored'`).Scan(&se); err != nil || se != nil {
		t.Fatalf("scored row score_error = (%v, %v), want untouched NULL", se, err)
	}

	// The batch closed and its membership freed inside the same transaction.
	if n, err := repo.CountOpenScoreBatches(); err != nil || n != 0 {
		t.Fatalf("open after apply = (%d, %v), want 0", n, err)
	}
	var members int
	if err := repo.db.QueryRow(`SELECT COUNT(*) FROM score_batch_jobs WHERE batch_name='batches/apply'`).Scan(&members); err != nil || members != 0 {
		t.Fatalf("members after apply = (%d, %v), want 0", members, err)
	}

	// Ledger landed once, on both the tenant and host rows.
	if n, err := repo.DailyAPIUsage("m-batch"); err != nil || n != 5 {
		t.Fatalf("tenant calls = (%d, %v), want 5", n, err)
	}
	if n, err := repo.HostDailyAPIUsage("m-batch"); err != nil || n != 5 {
		t.Fatalf("host calls = (%d, %v), want 5", n, err)
	}
	tok, err := repo.DailyAPITokens("m-batch")
	if err != nil || tok.Prompt != 500 || tok.CachedPrompt != 10 || tok.Candidates != 50 || tok.Total != 560 {
		t.Fatalf("tenant tokens = (%+v, %v)", tok, err)
	}

	// A racing re-apply claims nothing, writes nothing, ledgers nothing.
	applied, claimed, err = repo.ApplyBatchScoreResults("batches/apply", writes, "hash-1", 3, "m-batch", usage)
	if err != nil || claimed || applied != 0 {
		t.Fatalf("re-apply = (%d, %v, %v), want (0, false, nil)", applied, claimed, err)
	}
	if n, err := repo.DailyAPIUsage("m-batch"); err != nil || n != 5 {
		t.Fatalf("re-apply double-ledgered: (%d, %v), want 5", n, err)
	}
	assertJob("errored", nil, "pending")
}

// JobBriefs returns the found subset keyed by id; missing ids are absent and
// an empty input issues no query.
func TestJobBriefs(t *testing.T) {
	repo := newTestRepo(t)
	seedJob(t, repo, "b1", "T1", "Co")
	seedJob(t, repo, "b2", "T2", "Co")

	briefs, err := repo.JobBriefs([]string{"b1", "b2", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(briefs) != 2 || briefs["b1"].Title != "T1" || briefs["b2"].Title != "T2" {
		t.Fatalf("briefs = %+v", briefs)
	}
	if briefs, err := repo.JobBriefs(nil); err != nil || len(briefs) != 0 {
		t.Fatalf("empty briefs = (%+v, %v)", briefs, err)
	}
}
