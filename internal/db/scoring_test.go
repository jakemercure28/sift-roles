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
