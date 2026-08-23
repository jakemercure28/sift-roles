package db

import (
	"path/filepath"
	"testing"
)

// TestCloseCheckJobsSkipsApplied: the source-ATS liveness probe only declutters
// unapplied listings. Once a job is applied to or being interviewed for, a
// posting coming down (a repost under a new ATS id, or a board flap) must not
// auto-close the row out from under an in-flight application.
func TestCloseCheckJobsSkipsApplied(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	for _, l := range []struct{ id, stage string }{
		{"pending-row", ""},
		{"applied-row", "applied"},
		{"phone-row", "phone_screen"},
		{"interview-row", "interview"},
		{"offer-row", "offer"},
		{"rejected-row", "rejected"},
		{"closed-row", "closed"},
	} {
		if _, err := repo.InsertScrapedLead(distLead(l.id, "DevOps Engineer", "Acme", "Greenhouse")); err != nil {
			t.Fatalf("insert %s: %v", l.id, err)
		}
		if l.stage != "" {
			if err := repo.SetPipelineStage(l.id, l.stage); err != nil {
				t.Fatalf("stage %s: %v", l.id, err)
			}
		}
	}

	jobs, err := repo.CloseCheckJobs()
	if err != nil {
		t.Fatalf("CloseCheckJobs: %v", err)
	}
	got := map[string]bool{}
	for _, j := range jobs {
		got[j.ID] = true
	}

	if !got["pending-row"] {
		t.Errorf("pending row should be eligible for the liveness probe, got %v", got)
	}
	for _, id := range []string{"applied-row", "phone-row", "interview-row", "offer-row", "rejected-row", "closed-row"} {
		if got[id] {
			t.Errorf("protected row %s must not be probed for closure", id)
		}
	}
}
