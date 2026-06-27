package db

import (
	"strings"
	"testing"
)

func seedDescriptionJob(t *testing.T, r *Repository, id, desc, createdAt string) {
	t.Helper()
	_, err := r.db.Exec(`
		INSERT INTO jobs (id, title, company, url, platform, description, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`,
		id, "Platform Engineer", "Acme", "https://example.com/"+id, "greenhouse", desc, createdAt)
	if err != nil {
		t.Fatalf("seed description job: %v", err)
	}
}

func TestCheckDescriptionsClassifiesTodayJobs(t *testing.T) {
	repo := newTestRepo(t)
	seedDescriptionJob(t, repo, "critical", "", "2026-06-09 10:00:00")
	seedDescriptionJob(t, repo, "warn", strings.Repeat("x", 100), "2026-06-09 11:00:00")
	seedDescriptionJob(t, repo, "ok", strings.Repeat("x", 400), "2026-06-09 12:00:00")
	seedDescriptionJob(t, repo, "old", "", "2026-06-08 10:00:00")

	got, err := repo.CheckDescriptions("2026-06-09")
	if err != nil {
		t.Fatalf("CheckDescriptions: %v", err)
	}
	if got.Total != 3 || got.OK != 1 {
		t.Fatalf("health = %+v, want total 3 ok 1", got)
	}
	if len(got.Critical) != 1 || got.Critical[0].ID != "critical" {
		t.Fatalf("critical = %+v", got.Critical)
	}
	if len(got.Warn) != 1 || got.Warn[0].ID != "warn" {
		t.Fatalf("warn = %+v", got.Warn)
	}
	if got.Timestamp == "" {
		t.Fatal("timestamp should be set")
	}
}

func TestCheckDescriptionsNoRowsUsesEmptySlices(t *testing.T) {
	got, err := newTestRepo(t).CheckDescriptions("2026-06-09")
	if err != nil {
		t.Fatalf("CheckDescriptions: %v", err)
	}
	if got.Total != 0 || got.OK != 0 || len(got.Critical) != 0 || len(got.Warn) != 0 {
		t.Fatalf("health = %+v, want empty", got)
	}
	if got.Critical == nil || got.Warn == nil {
		t.Fatalf("critical/warn should be empty slices for JSON parity: %+v", got)
	}
}
