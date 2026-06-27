package db

import (
	"database/sql"
	"testing"
)

// insertJob seeds a jobs row with the columns the dedup/ghost passes read.
func insertJob(t *testing.T, r *Repository, id, title, company, platform, status string, score *int, stage, createdAt, appliedAt string) {
	t.Helper()
	// Empty stage/applied_at seed as NULL (matching real rows); empty created_at
	// defaults to now.
	_, err := r.db.Exec(
		`INSERT INTO jobs (id, title, company, url, platform, status, score, stage, created_at, applied_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), COALESCE(NULLIF(?, ''), datetime('now')), NULLIF(?, ''))`,
		id, title, company, "https://example.com/"+id, platform, status, score, stage, createdAt, appliedAt,
	)
	if err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func statusOf(t *testing.T, r *Repository, id string) (status, stage string) {
	t.Helper()
	if err := r.db.QueryRow(`SELECT status, COALESCE(stage,'') FROM jobs WHERE id=?`, id).Scan(&status, &stage); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return
}

func TestDedupExistingJobs(t *testing.T) {
	repo := newTestRepo(t)

	low := 3

	// Group 1 (reposts): a pending re-post of a genuinely reviewed-then-dismissed
	// job (archived, scored, no stage) is archived.
	insertJob(t, repo, "r-pending", "RP", "C1", "greenhouse", "pending", nil, "", "", "")
	insertJob(t, repo, "r-archived", "RP", "C1", "greenhouse", "archived", &low, "", "", "")

	// Group 2 (pending dupes): an aggregator and an ATS copy, both pending. The
	// ATS copy is kept regardless of recency so the survivor is canonical.
	insertJob(t, repo, "p-alt", "PD", "C2", "builtin", "pending", nil, "", "2026-06-02 00:00:00", "")
	insertJob(t, repo, "p-ats", "PD", "C2", "greenhouse", "pending", nil, "", "2026-06-01 00:00:00", "")

	// Group 3 (alternate vs primary): an aggregator dupe of a live primary ATS
	// row is archived; the primary is untouched.
	score := 8
	insertJob(t, repo, "alt", "AL", "C3", "remoteok", "pending", nil, "", "", "")
	insertJob(t, repo, "prim", "AL", "C3", "greenhouse", "applied", &score, "", "", "")

	// Group 4 (cascade guard): a pending copy whose only archived sibling is an
	// *unscored* dedup casualty must survive — the reposts pass must not treat a
	// deduped copy as a dismissal, or unique listings cascade to zero rows.
	insertJob(t, repo, "c-pending", "CG", "C4", "greenhouse", "pending", nil, "", "", "")
	insertJob(t, repo, "c-archived", "CG", "C4", "builtin", "archived", nil, "", "", "")

	// Group 5 (orphaned alternate): an aggregator whose only ATS sibling is
	// already archived must survive — there is no live primary to defer to.
	insertJob(t, repo, "o-alt", "OA", "C5", "remoteok", "pending", nil, "", "", "")
	insertJob(t, repo, "o-ats", "OA", "C5", "greenhouse", "archived", nil, "", "", "")

	res, err := repo.DedupExistingJobs()
	if err != nil {
		t.Fatalf("DedupExistingJobs: %v", err)
	}
	if res.Reposts != 1 || res.Pending != 1 || res.Alternates != 1 {
		t.Fatalf("counts = %+v, want reposts=1 pending=1 alternates=1", res)
	}

	for id, want := range map[string]string{
		"r-pending":  "archived",
		"r-archived": "archived",
		"p-alt":      "archived",
		"p-ats":      "pending",
		"alt":        "archived",
		"prim":       "applied",
		"c-pending":  "pending",
		"c-archived": "archived",
		"o-alt":      "pending",
		"o-ats":      "archived",
	} {
		if got, _ := statusOf(t, repo, id); got != want {
			t.Errorf("%s status = %q, want %q", id, got, want)
		}
	}

	// Every auto-archive records why, so the mutation is attributable.
	for id, want := range map[string]string{
		"r-pending": ArchiveReasonDedupRepost,
		"p-alt":     ArchiveReasonDedupPending,
		"alt":       ArchiveReasonDedupAlternate,
	} {
		var reason sql.NullString
		if err := repo.db.QueryRow(`SELECT archive_reason FROM jobs WHERE id=?`, id).Scan(&reason); err != nil {
			t.Fatalf("read reason %s: %v", id, err)
		}
		if reason.String != want {
			t.Errorf("%s archive_reason = %q, want %q", id, reason.String, want)
		}
	}

	// Idempotent: a second pass over the same table archives nothing more.
	again, err := repo.DedupExistingJobs()
	if err != nil {
		t.Fatalf("second DedupExistingJobs: %v", err)
	}
	if again.Total() != 0 {
		t.Errorf("second dedup pass archived %d rows, want 0 (not idempotent): %+v", again.Total(), again)
	}

	// Canary invariant: dedup must never leave a listing with zero scoreable
	// rows. This is the assertion that would have caught the cascade bug.
	if n, err := repo.CountCascadedArchives(); err != nil {
		t.Fatalf("CountCascadedArchives: %v", err)
	} else if n != 0 {
		t.Errorf("cascade canary = %d groups, want 0", n)
	}
}

// TestCountCascadedArchives_DetectsCascade proves the canary actually fires when
// a listing is left with every copy archived+unscored by a dedup pass.
func TestCountCascadedArchives_DetectsCascade(t *testing.T) {
	repo := newTestRepo(t)

	// A unique listing whose every row was dedup-archived (the cascade bug's
	// end state) trips the canary...
	insertJob(t, repo, "dead-1", "Z", "ZCo", "greenhouse", "archived", nil, "", "", "")
	insertJob(t, repo, "dead-2", "Z", "ZCo", "builtin", "archived", nil, "", "", "")
	if _, err := repo.db.Exec(`UPDATE jobs SET archive_reason=? WHERE id IN ('dead-1','dead-2')`, ArchiveReasonDedupAlternate); err != nil {
		t.Fatalf("seed reasons: %v", err)
	}
	// ...while a user-dismissed unscored job (NULL reason) does not.
	insertJob(t, repo, "dismissed", "Y", "YCo", "greenhouse", "archived", nil, "", "", "")

	n, err := repo.CountCascadedArchives()
	if err != nil {
		t.Fatalf("CountCascadedArchives: %v", err)
	}
	if n != 1 {
		t.Errorf("cascade canary = %d, want 1 (only the dedup-killed group)", n)
	}
}

func TestAutoGhostStale(t *testing.T) {
	repo := newTestRepo(t)

	// Stale applied job (>14 days, no progress) -> ghosted.
	insertJob(t, repo, "ghost-me", "G", "Co", "greenhouse", "applied", nil, "", "", "2026-01-01 00:00:00")
	// Recently applied -> kept.
	insertJob(t, repo, "recent", "R", "Co", "greenhouse", "applied", nil, "applied", "", "")
	// Applied but advanced past 'applied' -> kept (filtered by COALESCE(stage)).
	insertJob(t, repo, "advanced", "A", "Co", "greenhouse", "applied", nil, "interview", "", "2026-01-01 00:00:00")

	// "recent" needs a real recent applied_at; the seed used '' -> NULL, so set it.
	if _, err := repo.db.Exec(`UPDATE jobs SET applied_at=datetime('now') WHERE id='recent'`); err != nil {
		t.Fatalf("set recent applied_at: %v", err)
	}

	n, err := repo.AutoGhostStale(14)
	if err != nil {
		t.Fatalf("AutoGhostStale: %v", err)
	}
	if n != 1 {
		t.Fatalf("ghosted %d, want 1", n)
	}

	if st, stg := statusOf(t, repo, "ghost-me"); st != "ghosted" || stg != "ghosted" {
		t.Errorf("ghost-me = (%q,%q), want (ghosted,ghosted)", st, stg)
	}
	if st, _ := statusOf(t, repo, "recent"); st != "applied" {
		t.Errorf("recent status = %q, want applied", st)
	}
	if st, _ := statusOf(t, repo, "advanced"); st != "applied" {
		t.Errorf("advanced status = %q, want applied", st)
	}

	// A stage_change event was logged for the ghosted job.
	var n2 int
	if err := repo.db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE job_id='ghost-me' AND event_type='stage_change' AND to_value='ghosted' AND from_value='applied'`,
	).Scan(&n2); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n2 != 1 {
		t.Fatalf("ghost event count = %d, want 1", n2)
	}

	// Idempotent: nothing left to ghost.
	if again, err := repo.AutoGhostStale(14); err != nil || again != 0 {
		t.Fatalf("second AutoGhostStale = (%d,%v), want (0,nil)", again, err)
	}
}
