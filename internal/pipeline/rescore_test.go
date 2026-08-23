package pipeline

import (
	"context"
	"testing"

	"job-search-automation/internal/db"
	"job-search-automation/internal/scorer"
)

// archiveLowScored drives a job through a first-time scoring pass so it lands
// archived exactly the way the real pipeline archives a low score.
func archiveLowScored(t *testing.T, repo *db.Repository, id, title string, score int) {
	t.Helper()
	insertLead(t, repo, id, title)
	fake := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(score, "years shortfall"), nil }}
	if _, err := ScoreUnscored(context.Background(), repo, fake,
		ScoreConfig{DailyLimit: 500, Concurrency: 1, ArchiveThreshold: 4}, discardLogger()); err != nil {
		t.Fatalf("seed scoring pass: %v", err)
	}
}

// A re-score that clears the threshold must bring the row back to pending.
func TestRescoreJobsUnarchivesRecoveredRow(t *testing.T) {
	repo, path := newRepo(t)
	archiveLowScored(t, repo, "j1", "Cloud Infrastructure Engineer", 4)

	conn := verifyConn(t, path)
	if got := scalarStr(t, conn, "SELECT status FROM jobs WHERE id='j1'"); got != "archived" {
		t.Fatalf("setup: j1 status = %q, want archived", got)
	}

	jobs, err := repo.JobsToRescore([]string{"j1"}, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(8, "years are a soft signal"), nil }}
	out, err := RescoreJobs(context.Background(), repo, fake, jobs,
		ScoreConfig{DailyLimit: 500, Concurrency: 1, ArchiveThreshold: 4}, discardLogger())
	if err != nil {
		t.Fatalf("RescoreJobs: %v", err)
	}
	if out.ScoredOK != 1 || out.Unarchived != 1 {
		t.Fatalf("result = %+v, want 1 scored / 1 unarchived", out)
	}
	if got := scalarStr(t, conn, "SELECT status FROM jobs WHERE id='j1'"); got != "pending" {
		t.Fatalf("j1 status = %q, want pending", got)
	}
	if got := scalarInt(t, conn, "SELECT score FROM jobs WHERE id='j1'"); got != 8 {
		t.Fatalf("j1 score = %d, want 8", got)
	}
}

// A re-score that confirms the reject must leave the row archived.
func TestRescoreJobsKeepsConfirmedRejectArchived(t *testing.T) {
	repo, path := newRepo(t)
	archiveLowScored(t, repo, "j1", "Sales Engineer", 3)

	jobs, err := repo.JobsToRescore([]string{"j1"}, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(2, "different function"), nil }}
	out, err := RescoreJobs(context.Background(), repo, fake, jobs,
		ScoreConfig{DailyLimit: 500, Concurrency: 1, ArchiveThreshold: 4}, discardLogger())
	if err != nil {
		t.Fatalf("RescoreJobs: %v", err)
	}
	if out.Unarchived != 0 {
		t.Fatalf("unarchived = %d, want 0", out.Unarchived)
	}
	conn := verifyConn(t, path)
	if got := scalarStr(t, conn, "SELECT status FROM jobs WHERE id='j1'"); got != "archived" {
		t.Fatalf("j1 status = %q, want archived", got)
	}
	if got := scalarInt(t, conn, "SELECT score FROM jobs WHERE id='j1'"); got != 2 {
		t.Fatalf("j1 score = %d, want 2", got)
	}
}

// The whole point of scoring in place: a row must never sit in the
// (pending, score IS NULL) state that ArchiveStaleUnscored sweeps. If a
// re-score fails, the old score has to survive.
func TestRescoreJobsNeverNullsScoreOnFailure(t *testing.T) {
	repo, path := newRepo(t)
	archiveLowScored(t, repo, "j1", "Cloud Infrastructure Engineer", 4)

	jobs, err := repo.JobsToRescore([]string{"j1"}, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) {
		return scorer.Result{Score: nil, Reasoning: "unparsable"}, nil
	}}
	if _, err := RescoreJobs(context.Background(), repo, fake, jobs,
		ScoreConfig{DailyLimit: 500, Concurrency: 1, ArchiveThreshold: 4}, discardLogger()); err != nil {
		t.Fatalf("RescoreJobs: %v", err)
	}
	conn := verifyConn(t, path)
	if n := scalarInt(t, conn, "SELECT COUNT(*) FROM jobs WHERE id='j1' AND score IS NULL"); n != 0 {
		t.Fatal("a failed re-score nulled the score, exposing the row to the stale-unscored sweep")
	}
	if got := scalarInt(t, conn, "SELECT score FROM jobs WHERE id='j1'"); got != 4 {
		t.Fatalf("j1 score = %d, want the original 4 preserved", got)
	}
}

func TestRescoreJobsNoOpOnEmptySet(t *testing.T) {
	repo, _ := newRepo(t)
	fake := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { t.Fatal("scorer called"); return res(1, ""), nil }}
	out, err := RescoreJobs(context.Background(), repo, fake, nil, ScoreConfig{ArchiveThreshold: 4}, discardLogger())
	if err != nil || out.ScoredOK != 0 || out.Unarchived != 0 {
		t.Fatalf("got (%+v, %v), want a clean no-op", out, err)
	}
}
