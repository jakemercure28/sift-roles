package pipeline

import (
	"context"
	"testing"

	"job-search-automation/internal/db"
	"job-search-automation/internal/scorer"
)

// With UseExtracts on, a job whose global row has a current extract must reach
// the scorer as facts (no description payload dependence), a job without one
// must keep the description path, and every scored row must carry the resume
// hash stamp so a later resume edit can invalidate it.
func TestScoreUnscoredUsesExtractsAndStampsResumeHash(t *testing.T) {
	repo, path := newRepo(t)
	insertLead(t, repo, "j1", "Platform Engineer")
	insertLead(t, repo, "j2", "SRE")

	// Give j1 a current extract; j2 stays extract-less. insertLead writes
	// description "desc", so hashing it matches the harvested global row.
	if err := repo.UpsertGlobalJobExtract(db.GlobalJobExtract{
		GlobalJobID:     "j1",
		DescriptionHash: db.DescriptionHash("desc"),
		ExtractVersion:  scorer.ExtractVersion,
		Facts:           `{"field_function":"platform engineering"}`,
		Model:           scorer.Model,
	}); err != nil {
		t.Fatalf("upsert extract: %v", err)
	}

	seen := map[string]scorer.Job{}
	fake := &fakeScorer{fn: func(j scorer.Job) (scorer.Result, error) {
		seen[j.Title] = j
		return res(8, "strong"), nil
	}}

	cfg := ScoreConfig{
		DailyLimit: 500, Concurrency: 1, ArchiveThreshold: 4,
		UseExtracts: true, ResumeHash: "hash-v1",
	}
	out, err := ScoreUnscored(context.Background(), repo, fake, cfg, discardLogger())
	if err != nil {
		t.Fatalf("ScoreUnscored: %v", err)
	}
	if out.ScoredOK != 2 {
		t.Fatalf("result = %+v, want 2 ok", out)
	}
	if seen["Platform Engineer"].ExtractFacts == "" {
		t.Fatal("j1 should have been scored with extract facts")
	}
	if seen["SRE"].ExtractFacts != "" {
		t.Fatal("j2 has no extract and must score via description")
	}

	conn := verifyConn(t, path)
	for _, id := range []string{"j1", "j2"} {
		if got := scalarStr(t, conn, "SELECT resume_hash FROM jobs WHERE id='"+id+"'"); got != "hash-v1" {
			t.Fatalf("%s resume_hash = %q, want hash-v1", id, got)
		}
	}
}

// A stale extract (description changed since extraction) must NOT be fed to
// the scorer; the job falls back to its description.
func TestScoreUnscoredIgnoresStaleExtract(t *testing.T) {
	repo, _ := newRepo(t)
	insertLead(t, repo, "j1", "Platform Engineer")
	if err := repo.UpsertGlobalJobExtract(db.GlobalJobExtract{
		GlobalJobID:     "j1",
		DescriptionHash: "old-hash",
		ExtractVersion:  scorer.ExtractVersion,
		Facts:           `{"field_function":"stale"}`,
		Model:           scorer.Model,
	}); err != nil {
		t.Fatalf("upsert extract: %v", err)
	}

	var got scorer.Job
	fake := &fakeScorer{fn: func(j scorer.Job) (scorer.Result, error) {
		got = j
		return res(8, "strong"), nil
	}}
	if _, err := ScoreUnscored(context.Background(), repo, fake,
		ScoreConfig{DailyLimit: 500, Concurrency: 1, ArchiveThreshold: 4, UseExtracts: true}, discardLogger()); err != nil {
		t.Fatalf("ScoreUnscored: %v", err)
	}
	if got.ExtractFacts != "" {
		t.Fatal("stale extract must not be used for scoring")
	}
}

// RequeueStaleHighScores must un-score exactly the pending rows stamped with a
// different resume hash AND scored at or above the band floor: low-scoring
// stale rows keep their score (the mismatched hash is the staleness marker),
// unstamped rows and non-pending rows stay put, and CountStaleScores reports
// the kept stale rows for the banner.
func TestRequeueStaleHighScores(t *testing.T) {
	repo, path := newRepo(t)
	insertLead(t, repo, "stale-high", "A")
	insertLead(t, repo, "stale-low", "B")
	insertLead(t, repo, "stamped-current", "C")
	insertLead(t, repo, "unstamped", "D")

	if err := repo.UpdateJobScoreForResume("stale-high", 7, "r", "hash-v1"); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateJobScoreForResume("stale-low", 3, "r", "hash-v1"); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateJobScoreForResume("stamped-current", 8, "r", "hash-v2"); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateJobScore("unstamped", 6, "r"); err != nil {
		t.Fatal(err)
	}

	n, err := repo.RequeueStaleHighScores("hash-v2", 6)
	if err != nil {
		t.Fatalf("RequeueStaleHighScores: %v", err)
	}
	if n != 1 {
		t.Fatalf("requeued = %d, want 1 (only the stale high scorer)", n)
	}

	conn := verifyConn(t, path)
	if got := scalarInt(t, conn, "SELECT COUNT(*) FROM jobs WHERE id='stale-high' AND score IS NULL"); got != 1 {
		t.Fatal("stale-high should be un-scored")
	}
	if got := scalarInt(t, conn, "SELECT score FROM jobs WHERE id='stale-low'"); got != 3 {
		t.Fatalf("stale-low score = %d, want 3 (kept; below the rescore band)", got)
	}
	if got := scalarInt(t, conn, "SELECT score FROM jobs WHERE id='stamped-current'"); got != 8 {
		t.Fatalf("stamped-current score = %d, want 8 (untouched)", got)
	}
	if got := scalarInt(t, conn, "SELECT score FROM jobs WHERE id='unstamped'"); got != 6 {
		t.Fatalf("unstamped score = %d, want 6 (NULL hash rows never invalidate)", got)
	}

	// The kept stale row is exactly what the banner counts.
	if got, err := repo.CountStaleScores("hash-v2"); err != nil || got != 1 {
		t.Fatalf("CountStaleScores = (%d, %v), want (1, nil)", got, err)
	}
}
