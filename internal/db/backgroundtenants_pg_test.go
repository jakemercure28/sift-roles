package db

import (
	"os"
	"sort"
	"testing"

	"job-search-automation/internal/model"
)

// TestBackgroundTenantsUnion is the regression guard for the new-tenant bootstrap
// deadlock: a tenant that finished onboarding (resume.md + marker on disk) but
// owns no job rows must still appear in the background fan-out, unioned with the
// tenants that do own rows. Keying the fan-out on job rows alone stranded fresh
// tenants forever. Gated on JSA_PG_DSN (see TestPostgresSmoke).
func TestBackgroundTenantsUnion(t *testing.T) {
	dsn := os.Getenv("JSA_PG_DSN")
	if dsn == "" {
		t.Skip("set JSA_PG_DSN to a throwaway Postgres DSN to run the union test")
	}

	base, err := OpenPostgres(dsn, DefaultPoolConfig(), "", false)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { base.Close() })

	// tenant-withjobs owns a row; tenant-jobless is onboarded on disk but has none.
	withJobs := base.ForUser("bg-withjobs")
	t.Cleanup(func() {
		_, _ = base.RawDB().Exec(`DELETE FROM jobs WHERE user_id = $1`, "bg-withjobs")
	})
	lead := model.Lead{ID: "bg-1", JobLead: model.JobLead{
		Title: "DevOps", Company: "Acme", DirectApplyURL: "https://x/bg-1",
		ATSPlatformName: "greenhouse", Description: "infra role with enough description text to satisfy any analytics thresholds in place",
	}}
	if ok, err := withJobs.InsertScrapedLead(lead); err != nil || !ok {
		t.Fatalf("insert: ok=%v err=%v", ok, err)
	}

	dataDir := t.TempDir()
	writeProfile(t, dataDir, "bg-jobless", map[string]string{
		"resume.md":  "Onboarded but not yet scraped",
		".onboarded": "2026-06-15T00:00:00Z\n",
	})
	// A provisioned-but-unfinished tenant (resume only) must NOT be pulled in.
	writeProfile(t, dataDir, "bg-unfinished", map[string]string{
		"resume.md": "half way",
	})

	got, err := base.BackgroundTenants(dataDir)
	if err != nil {
		t.Fatalf("BackgroundTenants: %v", err)
	}
	sort.Strings(got)

	seen := map[string]bool{}
	for _, uid := range got {
		seen[uid] = true
	}
	if !seen["bg-withjobs"] {
		t.Errorf("BackgroundTenants %v missing the tenant with jobs", got)
	}
	if !seen["bg-jobless"] {
		t.Errorf("BackgroundTenants %v missing the onboarded jobless tenant (deadlock!)", got)
	}
	if seen["bg-unfinished"] {
		t.Errorf("BackgroundTenants %v included a half-onboarded tenant (resume only)", got)
	}

	// Tenants() alone (jobs-derived) must NOT include the jobless tenant: this is
	// exactly the gap BackgroundTenants closes.
	jobsOnly, err := base.Tenants()
	if err != nil {
		t.Fatalf("Tenants: %v", err)
	}
	for _, uid := range jobsOnly {
		if uid == "bg-jobless" {
			t.Fatal("Tenants() unexpectedly returned a jobless tenant; test precondition broken")
		}
	}
}
