package db

import (
	"strings"
	"testing"
)

func seedMarketJob(t *testing.T, r *Repository, id, title, status, stage string, descLen int) {
	t.Helper()
	desc := strings.Repeat("x", descLen)
	_, err := r.db.Exec(
		`INSERT INTO jobs (id, title, company, description, score, status, stage, applied_at) VALUES (?,?,?,?,?,?,?,?)`,
		id, title, "Co", desc, 7, status, nullIfEmptyStr(stage), nil,
	)
	if err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func nullIfEmptyStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func TestMarketJobQueries(t *testing.T) {
	repo := newTestRepo(t)
	// live with long desc
	seedMarketJob(t, repo, "live1", "Senior Engineer", "pending", "", 200)
	seedMarketJob(t, repo, "live2", "SRE", "applied", "phone_screen", 200)
	// terminal (excluded from live)
	seedMarketJob(t, repo, "arch", "Archived Role", "archived", "", 200)
	seedMarketJob(t, repo, "rej", "Rejected Role", "rejected", "rejected", 200)
	// live but short desc (excluded from research-count, included in seniority)
	seedMarketJob(t, repo, "short", "Short Desc", "pending", "", 50)

	live, err := repo.LiveMarketSeniorityJobs()
	if err != nil {
		t.Fatalf("live: %v", err)
	}
	// live cohort excludes archived/rejected: live1, live2, short.
	if got := idSet(live); len(got) != 3 || !got["live1"] || !got["live2"] || !got["short"] {
		t.Fatalf("live seniority jobs = %v, want live1/live2/short", got)
	}

	all, err := repo.AllTimeMarketSeniorityJobs()
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("all-time seniority jobs = %d, want 5", len(all))
	}

	liveCount, _ := repo.CountLiveMarketResearchJobs()
	if liveCount != 2 { // live1, live2 (long desc, non-terminal); short excluded
		t.Fatalf("live research count = %d, want 2", liveCount)
	}
	allCount, _ := repo.CountAllTimeMarketResearchJobs()
	if allCount != 4 { // all with desc>100: live1, live2, arch, rej
		t.Fatalf("all research count = %d, want 4", allCount)
	}
}

func idSet(jobs []MarketSeniorityJob) map[string]bool {
	m := map[string]bool{}
	for _, j := range jobs {
		m[j.ID] = true
	}
	return m
}
