package db

import "testing"

func TestRejectionSyncStatus(t *testing.T) {
	repo := newTestRepo(t)

	if _, found, err := repo.ReadRejectionSyncStatus(); err != nil || found {
		t.Fatalf("empty: found=%v err=%v", found, err)
	}

	if err := repo.WriteRejectionSyncStatus(RejectionSyncStatus{
		Status: "ok", Fetched: 12, Applied: 2, Ignored: 9, Unmatched: 1,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	st, found, err := repo.ReadRejectionSyncStatus()
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	if st.Status != "ok" || st.Applied != 2 || st.LastRunAt == "" {
		t.Fatalf("status = %+v", st)
	}

	if err := repo.WriteRejectionSyncStatus(RejectionSyncStatus{Status: "error", Error: "imap login failed"}); err != nil {
		t.Fatalf("write error: %v", err)
	}
	st, _, _ = repo.ReadRejectionSyncStatus()
	if st.Status != "error" || st.Error == "" {
		t.Fatalf("error status = %+v", st)
	}
}

func TestCountRejectionsAppliedSince(t *testing.T) {
	repo := newTestRepo(t)

	if n, err := repo.CountRejectionsAppliedSince(7); err != nil || n != 0 {
		t.Fatalf("empty count = %d err=%v", n, err)
	}

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := repo.RawDB().Exec(q, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	mustExec(`INSERT INTO rejection_email_log (mailbox, uid_validity, uid, match_status, created_at)
		VALUES ('m', 'v', 1, 'applied', datetime('now','-2 days'))`)
	mustExec(`INSERT INTO rejection_email_log (mailbox, uid_validity, uid, match_status, created_at)
		VALUES ('m', 'v', 2, 'ignored', datetime('now','-2 days'))`)
	mustExec(`INSERT INTO rejection_email_log (mailbox, uid_validity, uid, match_status, created_at)
		VALUES ('m', 'v', 3, 'applied', datetime('now','-30 days'))`)

	if n, err := repo.CountRejectionsAppliedSince(7); err != nil || n != 1 {
		t.Fatalf("count = %d err=%v, want 1", n, err)
	}
}
