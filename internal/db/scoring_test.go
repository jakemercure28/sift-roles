package db

import "testing"

func seedJob(t *testing.T, r *Repository, id, title, company string) {
	t.Helper()
	_, err := r.db.Exec(
		`INSERT INTO jobs (id, title, company, location, description, status)
		 VALUES (?, ?, ?, 'Remote', 'desc', 'pending')`,
		id, title, company,
	)
	if err != nil {
		t.Fatalf("seed job %s: %v", id, err)
	}
}

// seedStagedJob seeds an unscored job the user has already advanced out of
// 'pending' into a pipeline stage.
func seedStagedJob(t *testing.T, r *Repository, id, status, stage string) {
	t.Helper()
	_, err := r.db.Exec(
		`INSERT INTO jobs (id, title, company, location, description, status, stage)
		 VALUES (?, 'T', 'Co', 'Remote', 'desc', ?, ?)`,
		id, status, stage,
	)
	if err != nil {
		t.Fatalf("seed staged job %s: %v", id, err)
	}
}

// A job the user advanced into a live stage still needs a score, and it must
// outrank the pending backlog: it is the row whose blank score the user is
// actually looking at. Terminal stages stay excluded, since scoring a closed or
// rejected job spends quota on a settled decision.
func TestGetUnscoredJobsUserTouchedFirst(t *testing.T) {
	repo := newTestRepo(t)
	seedJob(t, repo, "pending-1", "A", "Co")
	seedJob(t, repo, "pending-2", "B", "Co")
	seedStagedJob(t, repo, "interviewing", "applied", "interview")
	seedStagedJob(t, repo, "phone", "applied", "phone_screen")
	seedStagedJob(t, repo, "responded", "responded", "")

	for _, tc := range []struct{ id, status, stage string }{
		{"closed", "applied", "closed"},
		{"rejected", "applied", "rejected"},
		{"ghosted", "applied", "ghosted"},
		{"archived", "archived", ""},
	} {
		seedStagedJob(t, repo, tc.id, tc.status, tc.stage)
	}

	jobs, err := repo.GetUnscoredJobs(0)
	if err != nil {
		t.Fatalf("GetUnscoredJobs: %v", err)
	}

	got := ids(jobs)
	if len(got) != 5 {
		t.Fatalf("got %d jobs %v, want 5 (2 pending + 3 live-stage)", len(got), got)
	}
	for _, id := range got[:3] {
		if id == "pending-1" || id == "pending-2" {
			t.Fatalf("pending job %q outranked a user-touched row, order = %v", id, got)
		}
	}
	for _, excluded := range []string{"closed", "rejected", "ghosted", "archived"} {
		for _, id := range got {
			if id == excluded {
				t.Fatalf("terminal row %q must not be scored, order = %v", excluded, got)
			}
		}
	}
}

// Triage must never archive a job out from under a user who is interviewing for
// it: they have already overruled the score by advancing it.
func TestAutoArchiveLowScoreSkipsUserTouched(t *testing.T) {
	repo := newTestRepo(t)
	seedStagedJob(t, repo, "interviewing", "applied", "interview")

	if err := repo.UpdateJobScore("interviewing", 2, "weak"); err != nil {
		t.Fatalf("score: %v", err)
	}
	if err := repo.AutoArchiveLowScore("interviewing", 4); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if got := status(t, repo, "interviewing"); got != "applied" {
		t.Fatalf("status = %q, want applied (low score must not archive it)", got)
	}
}

// The prefilter owns pending rows only. Its guards decline a user-touched row,
// and the caller relies on the reported false to keep the job rather than drop
// it from the pass.
func TestArchivePrefilteredReportsWhetherItLanded(t *testing.T) {
	repo := newTestRepo(t)
	seedJob(t, repo, "pending", "A", "Co")
	seedStagedJob(t, repo, "interviewing", "applied", "interview")

	archived, err := repo.ArchivePrefiltered("pending", ArchiveReasonPrefilterNonUS)
	if err != nil {
		t.Fatalf("archive pending: %v", err)
	}
	if !archived {
		t.Fatal("pending row should have been archived")
	}

	archived, err = repo.ArchivePrefiltered("interviewing", ArchiveReasonPrefilterNonUS)
	if err != nil {
		t.Fatalf("archive user-touched: %v", err)
	}
	if archived {
		t.Fatal("user-touched row must not be archived by the prefilter")
	}
	if got := status(t, repo, "interviewing"); got != "applied" {
		t.Fatalf("status = %q, want applied", got)
	}
}

func TestGetUnscoredJobsOrderAndLimit(t *testing.T) {
	repo := newTestRepo(t)

	// Three pending/unscored jobs; one already has an attempt, so it should sort
	// after the never-attempted ones.
	seedJob(t, repo, "a", "A", "Co")
	seedJob(t, repo, "b", "B", "Co")
	seedJob(t, repo, "c", "C", "Co")
	if err := repo.MarkScoreAttempt("a"); err != nil {
		t.Fatalf("attempt: %v", err)
	}

	// A scored job and a non-pending job must be excluded.
	seedJob(t, repo, "scored", "S", "Co")
	if err := repo.UpdateJobScore("scored", 9, "good"); err != nil {
		t.Fatalf("score: %v", err)
	}

	jobs, err := repo.GetUnscoredJobs(0)
	if err != nil {
		t.Fatalf("GetUnscoredJobs: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("got %d unscored, want 3", len(jobs))
	}
	// "a" was attempted, so it must come last.
	if jobs[len(jobs)-1].ID != "a" {
		t.Fatalf("attempted job should sort last, order = %v", ids(jobs))
	}

	limited, err := repo.GetUnscoredJobs(2)
	if err != nil {
		t.Fatalf("limit: %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("limit 2 returned %d", len(limited))
	}
}

// Rows with an empty, blank, or NULL description are excluded in SQL: neither
// scoring path can evaluate them, and before the pushdown they sat unstamped
// at the head of every capped fetch forever.
func TestGetUnscoredJobsExcludesEmptyDescriptions(t *testing.T) {
	repo := newTestRepo(t)
	seedJob(t, repo, "has-desc", "A", "Co")
	for id, desc := range map[string]any{"d-empty": "", "d-spaces": "   ", "d-null": nil} {
		if _, err := repo.db.Exec(
			`INSERT INTO jobs (id, title, company, location, description, status)
			 VALUES (?, 'T', 'Co', 'Remote', ?, 'pending')`, id, desc); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	jobs, err := repo.GetUnscoredJobs(0)
	if err != nil {
		t.Fatalf("GetUnscoredJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "has-desc" {
		t.Fatalf("unscored = %v, want only has-desc", ids(jobs))
	}
	if n, err := repo.CountUnscored(); err != nil || n != 4 {
		t.Fatalf("CountUnscored = (%d, %v), want all 4 (dashboard truth is unaffected)", n, err)
	}
}

func TestGetUnscoredJobsNewestPostedFirst(t *testing.T) {
	repo := newTestRepo(t)

	// Never-attempted rows sort newest-posted first; junk posted_at text must
	// not lexically outsort real dates, it falls back to created_at.
	seedJob(t, repo, "older", "A", "Co")
	seedJob(t, repo, "newest", "B", "Co")
	seedJob(t, repo, "junk", "C", "Co")
	set := func(id, postedAt, createdAt string) {
		t.Helper()
		if _, err := repo.db.Exec(`UPDATE jobs SET posted_at=NULLIF(?,''), created_at=? WHERE id=?`, postedAt, createdAt, id); err != nil {
			t.Fatalf("set %s: %v", id, err)
		}
	}
	set("older", "2026-06-01 00:00:00", "2026-07-01 00:00:00")
	set("newest", "2026-07-08 00:00:00", "2026-06-01 00:00:00")
	// Junk text with the oldest created_at: lexically "Posted..." > "2026-..."
	// so a raw DESC would put it first; the shape-guard must sort it last.
	set("junk", "Posted 6 Days Ago", "2026-05-01 00:00:00")

	jobs, err := repo.GetUnscoredJobs(0)
	if err != nil {
		t.Fatalf("GetUnscoredJobs: %v", err)
	}
	got := ids(jobs)
	want := []string{"newest", "older", "junk"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestUpdateJobScoreClearsError(t *testing.T) {
	repo := newTestRepo(t)
	seedJob(t, repo, "j", "J", "Co")

	if err := repo.MarkScoreFailure("j", "boom"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if err := repo.UpdateJobScore("j", 7, "solid"); err != nil {
		t.Fatalf("score: %v", err)
	}

	var score int
	var reasoning, scoreErr *string
	if err := repo.db.QueryRow(
		"SELECT score, reasoning, score_error FROM jobs WHERE id = 'j'",
	).Scan(&score, &reasoning, &scoreErr); err != nil {
		t.Fatalf("read: %v", err)
	}
	if score != 7 || reasoning == nil || *reasoning != "solid" {
		t.Fatalf("score=%d reasoning=%v", score, reasoning)
	}
	if scoreErr != nil {
		t.Fatalf("score_error should be cleared, got %q", *scoreErr)
	}
}

func TestAutoArchiveLowScore(t *testing.T) {
	repo := newTestRepo(t)
	seedJob(t, repo, "low", "Low", "Co")
	seedJob(t, repo, "high", "High", "Co")

	if err := repo.UpdateJobScore("low", 3, "weak"); err != nil {
		t.Fatalf("score low: %v", err)
	}
	if err := repo.UpdateJobScore("high", 9, "strong"); err != nil {
		t.Fatalf("score high: %v", err)
	}
	if err := repo.AutoArchiveLowScore("low", 4); err != nil {
		t.Fatalf("archive low: %v", err)
	}
	if err := repo.AutoArchiveLowScore("high", 4); err != nil {
		t.Fatalf("archive high: %v", err)
	}

	if got := status(t, repo, "low"); got != "archived" {
		t.Fatalf("low status = %q, want archived", got)
	}
	if got := status(t, repo, "high"); got != "pending" {
		t.Fatalf("high status = %q, want pending (above threshold)", got)
	}
}

func TestAPIUsageTracking(t *testing.T) {
	repo := newTestRepo(t)

	n, err := repo.DailyAPIUsage("m")
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if n != 0 {
		t.Fatalf("initial usage = %d, want 0", n)
	}

	for i := 0; i < 3; i++ {
		if err := repo.RecordAPICall("m"); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	n, err = repo.DailyAPIUsage("m")
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if n != 3 {
		t.Fatalf("usage = %d, want 3", n)
	}
}

func TestAPITokenUsageTracking(t *testing.T) {
	repo := newTestRepo(t)

	if err := repo.RecordAPITokens("m", 100, 40, 20, 120); err != nil {
		t.Fatalf("record tokens: %v", err)
	}
	if err := repo.RecordAPITokens("m", 50, 10, 5, 55); err != nil {
		t.Fatalf("record second tokens: %v", err)
	}
	got, err := repo.DailyAPITokens("m")
	if err != nil {
		t.Fatalf("daily tokens: %v", err)
	}
	want := APITokenUsage{Prompt: 150, CachedPrompt: 50, Candidates: 25, Total: 175}
	if got != want {
		t.Fatalf("tokens = %+v, want %+v", got, want)
	}

	if err := repo.RecordHostAPITokens("m", 7, 3, 2, 9); err != nil {
		t.Fatalf("record host tokens: %v", err)
	}
	host, err := repo.HostDailyAPITokens("m")
	if err != nil {
		t.Fatalf("host tokens: %v", err)
	}
	if host != (APITokenUsage{Prompt: 7, CachedPrompt: 3, Candidates: 2, Total: 9}) {
		t.Fatalf("host tokens = %+v", host)
	}

	// Token writes must not increment the call-count quota.
	n, err := repo.DailyAPIUsage("m")
	if err != nil {
		t.Fatalf("daily calls: %v", err)
	}
	if n != 0 {
		t.Fatalf("daily calls = %d, want 0", n)
	}
}

func ids(jobs []UnscoredJob) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.ID
	}
	return out
}

func status(t *testing.T, r *Repository, id string) string {
	t.Helper()
	var s string
	if err := r.db.QueryRow("SELECT status FROM jobs WHERE id = ?", id).Scan(&s); err != nil {
		t.Fatalf("status %s: %v", id, err)
	}
	return s
}
