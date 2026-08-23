package db

import (
	"testing"
)

// seedScored inserts a scored row with reasoning, description, and archive_reason.
func seedScored(t *testing.T, r *Repository, id, status string, score int, reasoning, archiveReason string) {
	t.Helper()
	_, err := r.db.Exec(
		`INSERT INTO jobs (id, title, company, url, platform, status, score, reasoning, description, archive_reason, created_at)
		 VALUES (?, ?, 'C', ?, 'greenhouse', ?, ?, ?, 'A real description.', NULLIF(?, ''), datetime('now'))`,
		id, "T-"+id, "https://example.com/"+id, status, score, reasoning, archiveReason,
	)
	if err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func idsOf(jobs []UnscoredJob) map[string]bool {
	out := map[string]bool{}
	for _, j := range jobs {
		out[j.ID] = true
	}
	return out
}

func TestJobsToRescoreByIDs(t *testing.T) {
	repo := newTestRepo(t)
	seedScored(t, repo, "a", "archived", 4, "Only 3 years, failing to meet the 4+ requirement.", ArchiveReasonLowScore)
	seedScored(t, repo, "b", "pending", 8, "Strong match.", "")

	got, err := repo.JobsToRescore([]string{"a", "b"}, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Explicit ids ignore score and status entirely.
	if len(got) != 2 {
		t.Fatalf("got %d jobs, want 2", len(got))
	}
}

func TestJobsToRescoreYearsOnly(t *testing.T) {
	repo := newTestRepo(t)
	seedScored(t, repo, "years", "archived", 4, "The candidate has only 3 years, failing to meet the role's 4+ requirement.", ArchiveReasonLowScore)
	seedScored(t, repo, "years-caps", "archived", 3, "Only 2 YEARS shown, FALLS SHORT of the minimum.", ArchiveReasonLowScore)
	// A real reject that merely mentions years must not be swept up... it lacks
	// any shortfall phrasing.
	seedScored(t, repo, "function", "archived", 2, "This is a sales role, a different function from the candidate's 4 years of infra work.", ArchiveReasonLowScore)
	// Above the score bound.
	seedScored(t, repo, "high", "pending", 8, "Only 3 years, falls short, but strong otherwise.", "")

	got, err := repo.JobsToRescore(nil, true, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := idsOf(got)
	if !ids["years"] || !ids["years-caps"] {
		t.Errorf("years-shortfall rows not selected: %v", ids)
	}
	if ids["function"] {
		t.Error("a function-mismatch reject was selected")
	}
	if ids["high"] {
		t.Error("a row above the score bound was selected")
	}
}

// A description-less row cannot be scored by either path, so it must never be
// selected: scoring one on its title alone inflates the score.
func TestJobsToRescoreSkipsDescriptionless(t *testing.T) {
	repo := newTestRepo(t)
	seedScored(t, repo, "nodesc", "archived", 4, "Only 3 years, falls short.", ArchiveReasonLowScore)
	if _, err := repo.db.Exec(`UPDATE jobs SET description='' WHERE id='nodesc'`); err != nil {
		t.Fatal(err)
	}
	got, err := repo.JobsToRescore(nil, true, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("selected a description-less row: %v", idsOf(got))
	}
}

func TestUnarchiveAboveThreshold(t *testing.T) {
	repo := newTestRepo(t)
	seedScored(t, repo, "recovered", "archived", 7, "Now a strong match.", ArchiveReasonLowScore)
	seedScored(t, repo, "still-low", "archived", 3, "Still a different function.", ArchiveReasonLowScore)
	// Archived by the user's own hand (NULL archive_reason): a re-score corrects
	// the model, never the user, so this must stay archived even at a high score.
	seedScored(t, repo, "user-archived", "archived", 9, "Great fit.", "")

	n, err := repo.UnarchiveAboveThreshold([]string{"recovered", "still-low", "user-archived"}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("unarchived %d rows, want 1", n)
	}
	if s, _ := statusOf(t, repo, "recovered"); s != "pending" {
		t.Errorf("recovered status = %q, want pending", s)
	}
	if s, _ := statusOf(t, repo, "still-low"); s != "archived" {
		t.Errorf("still-low status = %q, want archived", s)
	}
	if s, _ := statusOf(t, repo, "user-archived"); s != "archived" {
		t.Errorf("user-archived status = %q, want archived (user decision preserved)", s)
	}
}

func TestUnarchiveAboveThresholdEmptyIDs(t *testing.T) {
	repo := newTestRepo(t)
	n, err := repo.UnarchiveAboveThreshold(nil, 4)
	if err != nil || n != 0 {
		t.Fatalf("got (%d, %v), want (0, nil)", n, err)
	}
}
