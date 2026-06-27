package db

import (
	"os"
	"testing"

	"job-search-automation/internal/model"
)

// TestPostgresTenantIsolation is the load-bearing Phase-2 check: two tenants
// share one Postgres database, and neither can see or mutate the other's rows
// through any repository method. A query that forgot its user_id filter shows up
// here as cross-tenant leakage. Gated on JSA_PG_DSN (see TestPostgresSmoke).
func TestPostgresTenantIsolation(t *testing.T) {
	dsn := os.Getenv("JSA_PG_DSN")
	if dsn == "" {
		t.Skip("set JSA_PG_DSN to a throwaway Postgres DSN to run the isolation test")
	}

	base, err := OpenPostgres(dsn, DefaultPoolConfig(), "", false)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { base.Close() })

	a := base.ForUser("tenant-a")
	b := base.ForUser("tenant-b")

	lead := func(id, title string) model.Lead {
		return model.Lead{ID: id, JobLead: model.JobLead{
			Title: title, Company: "Acme", DirectApplyURL: "https://x/" + id,
			ATSPlatformName: "greenhouse", Description: "infra role with enough description text to count toward market research thresholds and analytics",
		}}
	}

	// Tenant A: 2 jobs. Tenant B: 1 job.
	for _, id := range []string{"iso-a1", "iso-a2"} {
		if ok, err := a.InsertScrapedLead(lead(id, "DevOps A")); err != nil || !ok {
			t.Fatalf("A insert %s: ok=%v err=%v", id, ok, err)
		}
	}
	if ok, err := b.InsertScrapedLead(lead("iso-b1", "DevOps B")); err != nil || !ok {
		t.Fatalf("B insert: ok=%v err=%v", ok, err)
	}

	// Counts are per tenant.
	if n, err := a.CountAllJobs(); err != nil || n != 2 {
		t.Fatalf("A CountAllJobs = %d (err %v), want 2", n, err)
	}
	if n, err := b.CountAllJobs(); err != nil || n != 1 {
		t.Fatalf("B CountAllJobs = %d (err %v), want 1", n, err)
	}

	// ActiveTenant picks the dominant tenant (A owns 2 jobs, B owns 1) so the
	// background pipeline resolves a single tenant's profile dir on Postgres.
	if uid, err := base.ActiveTenant(); err != nil || uid != "tenant-a" {
		t.Fatalf("ActiveTenant = %q (err %v), want tenant-a", uid, err)
	}

	// Tenants enumerates BOTH human tenants (and excludes the synthetic LocalUser),
	// so the background crons fan out per user instead of only the dominant one.
	tenants, err := base.Tenants()
	if err != nil {
		t.Fatalf("Tenants: %v", err)
	}
	seen := map[string]bool{}
	for _, uid := range tenants {
		if uid == LocalUser || uid == "" {
			t.Fatalf("Tenants returned reserved id %q", uid)
		}
		seen[uid] = true
	}
	if !seen["tenant-a"] || !seen["tenant-b"] || len(tenants) != 2 {
		t.Fatalf("Tenants = %v, want {tenant-a, tenant-b}", tenants)
	}

	// A cannot read B's job and vice versa.
	// jobs.id is tenant-scoped on Postgres (JobRowID), so probe each tenant's
	// actual stored row id rather than the raw scraper id.
	if _, found, err := a.JobDescription(b.JobRowID("iso-b1")); err != nil || found {
		t.Fatalf("A read B's job: found=%v err=%v (want not found)", found, err)
	}
	if _, found, err := b.JobDescription(a.JobRowID("iso-a1")); err != nil || found {
		t.Fatalf("B read A's job: found=%v err=%v (want not found)", found, err)
	}
	if _, found, err := a.JobDescription(a.JobRowID("iso-a1")); err != nil || !found {
		t.Fatalf("A read own job: found=%v err=%v (want found)", found, err)
	}

	// FilteredJobs is scoped.
	if rows, err := a.FilteredJobs("all", "created_at DESC"); err != nil || len(rows) != 2 {
		t.Fatalf("A FilteredJobs = %d rows (err %v), want 2", len(rows), err)
	}
	if rows, err := b.FilteredJobs("all", "created_at DESC"); err != nil || len(rows) != 1 {
		t.Fatalf("B FilteredJobs = %d rows (err %v), want 1", len(rows), err)
	}

	// company_notes: same company name, independent per-tenant rows (composite key).
	if err := a.SaveCompanyNotes("acme", "a-tag", "a-note"); err != nil {
		t.Fatalf("A SaveCompanyNotes: %v", err)
	}
	if err := b.SaveCompanyNotes("acme", "b-tag", "b-note"); err != nil {
		t.Fatalf("B SaveCompanyNotes: %v", err)
	}
	if tags, _, found, err := a.CompanyNotes("acme"); err != nil || !found || tags != "a-tag" {
		t.Fatalf("A CompanyNotes = %q found=%v err=%v, want a-tag", tags, found, err)
	}
	if tags, _, found, err := b.CompanyNotes("acme"); err != nil || !found || tags != "b-tag" {
		t.Fatalf("B CompanyNotes = %q found=%v err=%v, want b-tag", tags, found, err)
	}

	// api_usage: same date+model key, independent counts per tenant.
	for i := 0; i < 2; i++ {
		if err := a.RecordAPICall("gemini-flash"); err != nil {
			t.Fatalf("A RecordAPICall: %v", err)
		}
	}
	if err := b.RecordAPICall("gemini-flash"); err != nil {
		t.Fatalf("B RecordAPICall: %v", err)
	}
	if n, err := a.DailyAPIUsage("gemini-flash"); err != nil || n != 2 {
		t.Fatalf("A DailyAPIUsage = %d (err %v), want 2", n, err)
	}
	if n, err := b.DailyAPIUsage("gemini-flash"); err != nil || n != 1 {
		t.Fatalf("B DailyAPIUsage = %d (err %v), want 1", n, err)
	}

	// metadata: same key, independent values per tenant (heartbeat).
	if err := a.WriteHeartbeat("ok", 5, 2, ""); err != nil {
		t.Fatalf("A WriteHeartbeat: %v", err)
	}
	if err := b.WriteHeartbeat("error", 0, 0, "boom"); err != nil {
		t.Fatalf("B WriteHeartbeat: %v", err)
	}
	if hb, ok, err := a.ScraperHeartbeat(); err != nil || !ok || hb.Status != "ok" {
		t.Fatalf("A heartbeat = %+v ok=%v err=%v, want status ok", hb, ok, err)
	}
	if hb, ok, err := b.ScraperHeartbeat(); err != nil || !ok || hb.Status != "error" {
		t.Fatalf("B heartbeat = %+v ok=%v err=%v, want status error", hb, ok, err)
	}

	// Cross-tenant mutation is impossible: B cannot even see A's job, so staging
	// it fails (the job is not found for tenant B) and A's row is untouched.
	if err := b.SetPipelineStage("iso-a1", "applied"); err == nil {
		t.Fatal("B staged A's job; cross-tenant write should be impossible (leak!)")
	}
	applied, err := a.FilteredJobs("applied", "created_at DESC")
	if err != nil {
		t.Fatalf("A FilteredJobs applied: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("A has %d applied jobs after B's cross-tenant attempt, want 0 (leak!)", len(applied))
	}
}
