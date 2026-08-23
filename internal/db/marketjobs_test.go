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

	all, err := repo.AllTimeMarketSeniorityJobs(0)
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
	allCount, _ := repo.CountAllTimeMarketResearchJobs(0)
	if allCount != 4 { // all with desc>100: live1, live2, arch, rej
		t.Fatalf("all research count = %d, want 4", allCount)
	}
}

// TestMarketAllTimeWindow verifies the rolling-window bound: a stale row is
// excluded when a window is set but included when windowDays <= 0 (full scope).
func TestMarketAllTimeWindow(t *testing.T) {
	repo := newTestRepo(t)
	seedMarketJob(t, repo, "recent", "Senior Engineer", "pending", "", 200)
	// A row created ~400 days ago: outside a 180-day window, inside all-time.
	if _, err := repo.db.Exec(
		`INSERT INTO jobs (id, title, company, description, score, status, created_at)
		 VALUES (?,?,?,?,?,?, datetime('now','-400 days'))`,
		"stale", "Staff Engineer", "Co", strings.Repeat("x", 200), 7, "pending",
	); err != nil {
		t.Fatalf("seed stale: %v", err)
	}

	all, err := repo.AllTimeMarketSeniorityJobs(0)
	if err != nil {
		t.Fatalf("all-time (no window): %v", err)
	}
	if got := idSet(all); !got["recent"] || !got["stale"] {
		t.Fatalf("no-window all-time = %v, want recent+stale", got)
	}

	windowed, err := repo.AllTimeMarketSeniorityJobs(180)
	if err != nil {
		t.Fatalf("all-time (180d): %v", err)
	}
	if got := idSet(windowed); !got["recent"] || got["stale"] {
		t.Fatalf("180d all-time = %v, want recent only", got)
	}

	full, _ := repo.CountAllTimeMarketResearchJobs(0)
	if full != 2 {
		t.Fatalf("count no-window = %d, want 2", full)
	}
	bounded, _ := repo.CountAllTimeMarketResearchJobs(180)
	if bounded != 1 {
		t.Fatalf("count 180d = %d, want 1", bounded)
	}
}

func idSet(jobs []MarketSeniorityJob) map[string]bool {
	m := map[string]bool{}
	for _, j := range jobs {
		m[j.ID] = true
	}
	return m
}
