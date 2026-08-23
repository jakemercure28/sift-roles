package db

import (
	"os"
	"testing"
	"time"

	"job-search-automation/internal/model"
)

// TestScoreBatchPostgres exercises the score-batch ledger on the Postgres
// dialect: tenant scoping of create/close/apply (tenant B must not be able to
// close or apply onto tenant A's batch/jobs), the cross-tenant OpenScoreBatches
// read the poll loop uses, the sync-queue exclusion under the pg rewrite, and
// the RLS-enforced (rls=true) code path for the tenant-scoped writes. Like
// TestPostgresRLSPath this asserts the code path, not the policy (JSA_PG_DSN is
// typically a privileged role). Gated on JSA_PG_DSN.
func TestScoreBatchPostgres(t *testing.T) {
	dsn := os.Getenv("JSA_PG_DSN")
	if dsn == "" {
		t.Skip("set JSA_PG_DSN to a throwaway Postgres DSN to run the score-batch pg test")
	}

	admin, err := OpenPostgres(dsn, DefaultPoolConfig(), "", false)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { admin.Close() })
	t.Cleanup(func() {
		for _, tbl := range []string{"score_batch_jobs", "score_batches"} {
			_, _ = admin.RawDB().Exec(`DELETE FROM ` + tbl + ` WHERE user_id LIKE 'sb-tenant-%'`)
		}
		_, _ = admin.RawDB().Exec(`DELETE FROM jobs WHERE user_id LIKE 'sb-tenant-%'`)
	})

	lead := func(id string) model.Lead {
		return model.Lead{ID: id, JobLead: model.JobLead{
			Title: "DevOps", Company: "BatchCo", DirectApplyURL: "https://x/" + id,
			ATSPlatformName: "greenhouse", Description: "infra role with enough description text for analytics",
		}}
	}
	a := admin.ForUser("sb-tenant-a")
	b := admin.ForUser("sb-tenant-b")
	for _, seed := range []struct {
		r  *Repository
		id string
	}{{a, "sb-a1"}, {a, "sb-a2"}, {b, "sb-b1"}} {
		if ok, err := seed.r.InsertScrapedLead(lead(seed.id)); err != nil || !ok {
			t.Fatalf("seed %s: ok=%v err=%v", seed.id, ok, err)
		}
	}
	aJob1, aJob2 := a.JobRowID("sb-a1"), a.JobRowID("sb-a2")

	// Tenant-scoped writes through the RLS-enforced path (inTx stamps the GUC).
	serving, err := OpenPostgres(dsn, DefaultPoolConfig(), "", true)
	if err != nil {
		t.Fatalf("OpenPostgres serving: %v", err)
	}
	t.Cleanup(func() { serving.Close() })
	sa := serving.ForUser("sb-tenant-a")

	if err := sa.CreateScoreBatch("batches/pg-a", "m-batch", "hash", []string{aJob1, aJob2}); err != nil {
		t.Fatalf("CreateScoreBatch (rls path): %v", err)
	}

	// Sync-queue exclusion under the pg rewrite: only the non-member remains.
	unscored, err := a.GetUnscoredJobs(0)
	if err != nil {
		t.Fatalf("GetUnscoredJobs: %v", err)
	}
	if len(unscored) != 0 {
		t.Fatalf("tenant A unscored = %+v, want none (both in open batch)", unscored)
	}
	// Blank out one tenant-B description to prove the empty-description
	// pushdown under the pg rewrite: the row must vanish from the fetch.
	if _, err := admin.RawDB().Exec(`UPDATE jobs SET description = '' WHERE id = $1 AND user_id = 'sb-tenant-b'`, b.JobRowID("sb-b1")); err != nil {
		t.Fatalf("blank sb-b1 description: %v", err)
	}
	bUnscored, err := b.GetUnscoredJobs(0)
	if err != nil || len(bUnscored) != 0 {
		t.Fatalf("tenant B unscored = (%+v, %v), want none (empty description excluded)", bUnscored, err)
	}
	if _, err := admin.RawDB().Exec(`UPDATE jobs SET description = 'infra role, restored body' WHERE id = $1 AND user_id = 'sb-tenant-b'`, b.JobRowID("sb-b1")); err != nil {
		t.Fatalf("restore sb-b1 description: %v", err)
	}
	if bUnscored, err = b.GetUnscoredJobs(0); err != nil || len(bUnscored) != 1 {
		t.Fatalf("tenant B unscored = (%+v, %v), want its own 1", bUnscored, err)
	}

	// The poll loop's cross-tenant read sees the batch with its owner (any
	// repo works: OpenScoreBatches is deliberately unscoped, service-role only).
	open, err := b.OpenScoreBatches()
	if err != nil {
		t.Fatalf("OpenScoreBatches: %v", err)
	}
	var found bool
	for _, batch := range open {
		if batch.BatchName == "batches/pg-a" {
			found = batch.UserID == "sb-tenant-a" && batch.JobCount == 2
		}
	}
	if !found {
		t.Fatalf("open batches missing pg-a with owner: %+v", open)
	}

	// Tenant B cannot close or apply onto tenant A's batch: the close is a
	// no-op and the apply's claim misses, writing nothing.
	if err := b.CloseScoreBatch("batches/pg-a", ScoreBatchFailed, "not mine"); err != nil {
		t.Fatalf("cross-tenant close errored: %v", err)
	}
	if n, err := a.CountOpenScoreBatches(); err != nil || n != 1 {
		t.Fatalf("batch closed cross-tenant: (%d, %v)", n, err)
	}
	notMine := []BatchScoreWrite{{JobID: aJob1, Score: 9, Reasoning: "not mine"}}
	if applied, claimed, err := b.ApplyBatchScoreResults("batches/pg-a", notMine, "h", 3, "m-batch", BatchUsageTotals{}); err != nil || claimed || applied != 0 {
		t.Fatalf("cross-tenant apply = (%d, %v, %v), want (0, false, nil)", applied, claimed, err)
	}
	if n, err := a.CountOpenScoreBatches(); err != nil || n != 1 {
		t.Fatalf("batch claimed cross-tenant: (%d, %v)", n, err)
	}

	// Owner applies and closes in one transaction (rls path); member rows
	// free the exclusion.
	mine := []BatchScoreWrite{{JobID: aJob1, Score: 7, Reasoning: "batch scored"}}
	if applied, claimed, err := sa.ApplyBatchScoreResults("batches/pg-a", mine, "hash", 3, "m-batch", BatchUsageTotals{}); err != nil || !claimed || applied != 1 {
		t.Fatalf("owner apply = (%d, %v, %v), want (1, true, nil)", applied, claimed, err)
	}
	unscored, err = a.GetUnscoredJobs(0)
	if err != nil || len(unscored) != 1 || unscored[0].ID != aJob2 {
		t.Fatalf("after close unscored = (%+v, %v), want just the unapplied member", unscored, err)
	}

	// A wedged open batch older than the exclusion bound stops counting
	// against the submit gate under the pg rewrite, while the oldest-open
	// signal (deliberately unbounded) still sees it.
	if err := sa.CreateScoreBatch("batches/pg-wedged", "m-batch", "hash", []string{aJob2}); err != nil {
		t.Fatalf("CreateScoreBatch wedged: %v", err)
	}
	if n, err := a.CountOpenScoreBatches(); err != nil || n != 1 {
		t.Fatalf("fresh wedged count = (%d, %v), want 1", n, err)
	}
	old := time.Now().UTC().Add(-OpenBatchExclusionMax - time.Hour).Format("2006-01-02 15:04:05")
	if _, err := admin.RawDB().Exec(`UPDATE score_batches SET created_at = $1 WHERE batch_name = $2`, old, "batches/pg-wedged"); err != nil {
		t.Fatalf("backdate wedged: %v", err)
	}
	if n, err := a.CountOpenScoreBatches(); err != nil || n != 0 {
		t.Fatalf("stale wedged count = (%d, %v), want 0", n, err)
	}
	if created, err := a.OldestOpenScoreBatchCreatedAt(); err != nil || created != old {
		t.Fatalf("oldest open = (%q, %v), want %q", created, err, old)
	}

	// The admin CLI's lookups are cross-tenant: a repo scoped to tenant B
	// still sees tenant A's wedged row (with its owner), and RecentScoreBatches
	// keeps the ancient open row even when the window excludes its created_at.
	wedged, foundWedged, err := b.GetScoreBatchByName("batches/pg-wedged")
	if err != nil || !foundWedged || wedged.UserID != "sb-tenant-a" || wedged.State != ScoreBatchSubmitted {
		t.Fatalf("cross-tenant lookup = (%+v, %v, %v), want tenant A's open row", wedged, foundWedged, err)
	}
	sinceNow := time.Now().UTC().Add(-time.Hour).Format("2006-01-02 15:04:05")
	recent, err := b.RecentScoreBatches(sinceNow)
	if err != nil {
		t.Fatalf("RecentScoreBatches: %v", err)
	}
	var sawWedged bool
	for _, rb := range recent {
		sawWedged = sawWedged || rb.BatchName == "batches/pg-wedged"
	}
	if !sawWedged {
		t.Fatalf("recent batches missing the old open row: %+v", recent)
	}
}
