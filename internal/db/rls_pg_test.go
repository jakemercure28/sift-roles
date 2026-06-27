package db

import (
	"os"
	"testing"
	"time"

	"job-search-automation/internal/model"
)

// TestPostgresRLSPath exercises the RLS-enforced code path (rls=true): every
// exec/query/queryRow/inTx wraps its statement in a transaction that first sets the
// app.user_id GUC. It validates two things that don't need a restricted role:
//
//  1. Correctness: with RLS on, app-layer user_id scoping still returns the right
//     per-tenant rows through the rlsRows (query), rlsRow (queryRow), and inTx paths.
//  2. No connection leak: the per-query transactions are committed when the caller
//     closes the cursor / scans the row, so a tiny pool (MaxOpenConns=2) survives
//     many sequential queries instead of dead-locking on a leaked transaction.
//
// The actual fail-closed guarantee (an unscoped query returning zero rows) needs the
// restricted, non-BYPASSRLS role from docs/DEPLOY-thinkpad.md and is validated in
// staging/prod after the flip; JSA_PG_DSN here is typically a privileged role, which
// BYPASSes the policies, so this test deliberately asserts the code path, not the
// policy. Gated on JSA_PG_DSN.
func TestPostgresRLSPath(t *testing.T) {
	dsn := os.Getenv("JSA_PG_DSN")
	if dsn == "" {
		t.Skip("set JSA_PG_DSN to a throwaway Postgres DSN to run the RLS path test")
	}

	// Admin connection (rls=false) runs migrations and seeds, the way the service
	// role does in production.
	admin, err := OpenPostgres(dsn, DefaultPoolConfig(), "", false)
	if err != nil {
		t.Fatalf("OpenPostgres admin: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	lead := func(id, title string) model.Lead {
		return model.Lead{ID: id, JobLead: model.JobLead{
			Title: title, Company: "RlsCo", DirectApplyURL: "https://x/" + id,
			ATSPlatformName: "greenhouse", Description: "infra role with enough description text for analytics",
		}}
	}
	a := admin.ForUser("rls-tenant-a")
	b := admin.ForUser("rls-tenant-b")
	for _, id := range []string{"rls-a1", "rls-a2"} {
		if ok, err := a.InsertScrapedLead(lead(id, "DevOps A")); err != nil || !ok {
			t.Fatalf("seed A %s: ok=%v err=%v", id, ok, err)
		}
	}
	if ok, err := b.InsertScrapedLead(lead("rls-b1", "DevOps B")); err != nil || !ok {
		t.Fatalf("seed B: ok=%v err=%v", ok, err)
	}

	// Serving connection (rls=true) on a deliberately tiny pool: a leaked per-query
	// transaction would exhaust it within a few iterations and dead-lock the test.
	serving, err := OpenPostgres(dsn, PoolConfig{
		MaxOpenConns: 2, MaxIdleConns: 2,
		ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute,
	}, "", true)
	if err != nil {
		t.Fatalf("OpenPostgres serving (rls): %v", err)
	}
	t.Cleanup(func() { serving.Close() })

	sa := serving.ForUser("rls-tenant-a")

	// rlsRow path (queryRow): a count commits its transaction so the next query can
	// reuse the connection.
	if n, err := sa.CountAllJobs(); err != nil || n != 2 {
		t.Fatalf("rls CountAllJobs = %d (err %v), want 2", n, err)
	}
	// rlsRow path with sql.ErrNoRows: cross-tenant read returns not-found and still
	// releases the connection.
	// jobs.id is tenant-scoped on Postgres (JobRowID); probe the real row ids.
	if _, found, err := sa.JobDescription(b.JobRowID("rls-b1")); err != nil || found {
		t.Fatalf("rls cross-tenant read: found=%v err=%v (want not found)", found, err)
	}
	if _, found, err := sa.JobDescription(a.JobRowID("rls-a1")); err != nil || !found {
		t.Fatalf("rls own read: found=%v err=%v (want found)", found, err)
	}

	// rlsRows path (query) + leak check: run well past MaxOpenConns. If Close did not
	// commit the transaction, Begin would block here once the pool is drained.
	for i := 0; i < 25; i++ {
		rows, err := sa.FilteredJobs("all", "created_at DESC")
		if err != nil {
			t.Fatalf("rls FilteredJobs iter %d: %v", i, err)
		}
		if len(rows) != 2 {
			t.Fatalf("rls FilteredJobs iter %d = %d rows, want 2", i, len(rows))
		}
	}

	// inTx path: a write transaction also stamps the GUC and commits.
	if err := sa.SaveCompanyNotes("rlsco", "tag-a", "note-a"); err != nil {
		t.Fatalf("rls inTx SaveCompanyNotes: %v", err)
	}
}
