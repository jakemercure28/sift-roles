package pipeline

import (
	"context"
	"testing"

	"job-search-automation/internal/db"
	"job-search-automation/internal/jobfilter"
	"job-search-automation/internal/model"
	"job-search-automation/internal/scorer"
)

func insertLeadAt(t *testing.T, repo *db.Repository, id, title, location string) {
	t.Helper()
	lead := model.Lead{
		JobLead: model.JobLead{
			Title:            title,
			Company:          "Co",
			Description:      "desc",
			DirectApplyURL:   "https://example.com/" + id,
			ATSPlatformName:  "Greenhouse",
			ScrapedTimestamp: "2026-06-07T00:00:00.000Z",
			Location:         location,
			PostedAt:         "2026-06-06",
		},
		ID: id,
	}
	if _, err := repo.InsertScrapedLead(lead); err != nil {
		t.Fatalf("insert lead %s: %v", id, err)
	}
}

// The prefilter must archive location-ineligible jobs with the right
// archive_reason and never send them to the scorer; eligible jobs score as
// before.
func TestScoreUnscoredPrefiltersByLocation(t *testing.T) {
	repo, path := newRepo(t)
	insertLeadAt(t, repo, "j1", "Platform Engineer", "Remote, US")
	insertLeadAt(t, repo, "j2", "Platform Engineer", "London, UK")
	insertLeadAt(t, repo, "j3", "Platform Engineer", "Remote, United Kingdom")

	var scoredTitles []string
	fake := &fakeScorer{fn: func(j scorer.Job) (scorer.Result, error) {
		scoredTitles = append(scoredTitles, j.Location)
		return res(8, "strong"), nil
	}}

	prefs := jobfilter.Prefs{RemoteOnly: true, IncludeUnknown: true}
	cfg := ScoreConfig{
		DailyLimit: 500, Concurrency: 1, ArchiveThreshold: 4,
		Prefilter: func(location, title string) (bool, string) {
			return jobfilter.Evaluate(location, title, prefs)
		},
	}

	out, err := ScoreUnscored(context.Background(), repo, fake, cfg, discardLogger())
	if err != nil {
		t.Fatalf("ScoreUnscored: %v", err)
	}
	if out.Prefiltered != 2 || out.ScoredOK != 1 {
		t.Fatalf("result = %+v, want 2 prefiltered / 1 ok", out)
	}
	if len(scoredTitles) != 1 || scoredTitles[0] != "Remote, US" {
		t.Fatalf("scorer saw %v, want only the US remote job", scoredTitles)
	}

	conn := verifyConn(t, path)
	if got := scalarStr(t, conn, "SELECT status FROM jobs WHERE id='j2'"); got != "archived" {
		t.Fatalf("j2 status = %q, want archived", got)
	}
	if got := scalarStr(t, conn, "SELECT archive_reason FROM jobs WHERE id='j2'"); got != db.ArchiveReasonPrefilterLocation {
		t.Fatalf("j2 archive_reason = %q, want %q", got, db.ArchiveReasonPrefilterLocation)
	}
	if got := scalarStr(t, conn, "SELECT archive_reason FROM jobs WHERE id='j3'"); got != db.ArchiveReasonPrefilterNonUS {
		t.Fatalf("j3 archive_reason = %q, want %q", got, db.ArchiveReasonPrefilterNonUS)
	}
	if got := scalarStr(t, conn, "SELECT status FROM jobs WHERE id='j1'"); got != "pending" {
		t.Fatalf("j1 status = %q, want pending", got)
	}
	// Prefiltered rows must stay unscored: they were never sent to Gemini.
	if n := scalarInt(t, conn, "SELECT COUNT(*) FROM jobs WHERE score IS NOT NULL"); n != 1 {
		t.Fatalf("%d scored rows, want 1", n)
	}
}

// A nil prefilter (flag off or default prefs) must not change scoring behavior.
func TestScoreUnscoredNilPrefilterScoresEverything(t *testing.T) {
	repo, path := newRepo(t)
	insertLeadAt(t, repo, "j1", "Platform Engineer", "London, UK")

	fake := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(8, "strong"), nil }}
	out, err := ScoreUnscored(context.Background(), repo, fake,
		ScoreConfig{DailyLimit: 500, Concurrency: 1, ArchiveThreshold: 4}, discardLogger())
	if err != nil {
		t.Fatalf("ScoreUnscored: %v", err)
	}
	if out.Prefiltered != 0 || out.ScoredOK != 1 {
		t.Fatalf("result = %+v, want 0 prefiltered / 1 ok", out)
	}
	conn := verifyConn(t, path)
	if got := scalarInt(t, conn, "SELECT score FROM jobs WHERE id='j1'"); got != 8 {
		t.Fatalf("j1 score = %d, want 8", got)
	}
}
