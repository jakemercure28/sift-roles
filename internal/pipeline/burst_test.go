package pipeline

import (
	"context"
	"testing"

	"job-search-automation/internal/scorer"
)

func TestBurstIgnoresPerTenantCap(t *testing.T) {
	repo, path := newRepo(t)
	insertLead(t, repo, "b1", "Marketing Manager")

	// The tenant has already spent past the per-tenant fairness cap today. A cron
	// pass would score nothing; the onboarding burst must score anyway.
	for i := 0; i < 5; i++ {
		if err := repo.RecordAPICall(scorer.Model); err != nil {
			t.Fatalf("record api call: %v", err)
		}
		if err := repo.RecordHostAPICall(scorer.Model); err != nil {
			t.Fatalf("record host api call: %v", err)
		}
	}

	fs := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(8, "fits"), nil }}
	cfg := ScoreConfig{
		DailyLimit:              500,
		Concurrency:             1,
		ArchiveThreshold:        4,
		HostKey:                 true,
		HostDailyLimit:          100,
		HostPerTenantDailyLimit: 3, // already exceeded by the 5 recorded calls
	}
	out, err := ScoreOnboardingBurst(context.Background(), repo, fs, []string{"marketing"}, cfg, BurstConfig{Limit: 10}, discardLogger())
	if err != nil {
		t.Fatalf("ScoreOnboardingBurst: %v", err)
	}
	if out.ScoredOK != 1 {
		t.Fatalf("scored_ok = %d, want 1 (per-tenant cap must not gate the burst)", out.ScoredOK)
	}
	conn := verifyConn(t, path)
	if got := scalarInt(t, conn, "SELECT score FROM jobs WHERE id='b1'"); got != 8 {
		t.Fatalf("b1 score = %d, want 8", got)
	}
}

func TestBurstClampedByHostGlobalCap(t *testing.T) {
	repo, _ := newRepo(t)
	insertLead(t, repo, "b1", "Marketing Manager")

	// The host key's global budget is fully spent: the burst must not push the
	// shared key past the real provider quota, allowance or not.
	for i := 0; i < 3; i++ {
		if err := repo.RecordHostAPICall(scorer.Model); err != nil {
			t.Fatalf("record host api call: %v", err)
		}
	}

	fs := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(8, "fits"), nil }}
	cfg := ScoreConfig{Concurrency: 1, ArchiveThreshold: 4, HostKey: true, HostDailyLimit: 3}
	out, err := ScoreOnboardingBurst(context.Background(), repo, fs, nil, cfg, BurstConfig{Limit: 10}, discardLogger())
	if err != nil {
		t.Fatalf("ScoreOnboardingBurst: %v", err)
	}
	if out.Scored != 0 || fs.calls != 0 {
		t.Fatalf("scored = %d calls = %d, want 0/0 when the host budget is gone", out.Scored, fs.calls)
	}
}

func TestBurstPartialHostBudgetShrinksLimit(t *testing.T) {
	repo, _ := newRepo(t)
	insertLead(t, repo, "b1", "Marketing Manager")
	insertLead(t, repo, "b2", "Marketing Lead")
	insertLead(t, repo, "b3", "Marketing Director")

	// Only 2 host calls remain today; a limit-3 burst must shrink to 2.
	if err := repo.RecordHostAPICall(scorer.Model); err != nil {
		t.Fatalf("record host api call: %v", err)
	}
	fs := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(7, "fits"), nil }}
	cfg := ScoreConfig{Concurrency: 1, ArchiveThreshold: 4, HostKey: true, HostDailyLimit: 3}
	out, err := ScoreOnboardingBurst(context.Background(), repo, fs, nil, cfg, BurstConfig{Limit: 3}, discardLogger())
	if err != nil {
		t.Fatalf("ScoreOnboardingBurst: %v", err)
	}
	if out.ScoredOK != 2 {
		t.Fatalf("scored_ok = %d, want 2 (clamped to remaining host budget)", out.ScoredOK)
	}
}

func TestBurstArchivesLowScores(t *testing.T) {
	repo, path := newRepo(t)
	insertLead(t, repo, "junk", "Marketing Manager")

	fs := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(2, "poor fit"), nil }}
	cfg := ScoreConfig{DailyLimit: 500, Concurrency: 1, ArchiveThreshold: 4}
	if _, err := ScoreOnboardingBurst(context.Background(), repo, fs, nil, cfg, BurstConfig{Limit: 5}, discardLogger()); err != nil {
		t.Fatalf("ScoreOnboardingBurst: %v", err)
	}
	conn := verifyConn(t, path)
	if got := scalarStr(t, conn, "SELECT status FROM jobs WHERE id='junk'"); got != "archived" {
		t.Fatalf("junk status = %q, want archived", got)
	}
}

func TestBurstZeroLimitIsNoop(t *testing.T) {
	repo, _ := newRepo(t)
	insertLead(t, repo, "b1", "Marketing Manager")

	fs := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(8, "fits"), nil }}
	out, err := ScoreOnboardingBurst(context.Background(), repo, fs, nil, ScoreConfig{Concurrency: 1}, BurstConfig{}, discardLogger())
	if err != nil {
		t.Fatalf("ScoreOnboardingBurst: %v", err)
	}
	if out.Scored != 0 || fs.calls != 0 {
		t.Fatalf("scored = %d calls = %d, want 0/0 for a zero limit", out.Scored, fs.calls)
	}
}
