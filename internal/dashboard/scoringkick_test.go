package dashboard

import (
	"testing"
	"time"
)

// TestScoringKickFiresForReturningTenant covers the three stamp states the
// kick inspects: missing (fires), fresh (does not fire), stale (fires).
func TestScoringKickFiresForReturningTenant(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	srv, err := New(t.TempDir(), repo, nil, time.Second, 500, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	kicked := make(chan string, 1)
	srv.SetScoringKick(7, func(uid string) { kicked <- uid })

	waitKick := func() (string, bool) {
		select {
		case uid := <-kicked:
			return uid, true
		case <-time.After(2 * time.Second):
			return "", false
		}
	}

	// No stamp yet: a load kicks.
	srv.maybeKickScoring()
	if uid, ok := waitKick(); !ok {
		t.Fatal("kick did not fire for never-seen tenant")
	} else if uid != repo.UserID() {
		t.Fatalf("kick uid = %q, want %q", uid, repo.UserID())
	}

	// Fresh stamp: no kick.
	if err := repo.TouchLastSeen(); err != nil {
		t.Fatalf("touch: %v", err)
	}
	srv.maybeKickScoring()
	select {
	case <-kicked:
		t.Fatal("kick fired despite fresh stamp")
	case <-time.After(150 * time.Millisecond):
	}

	// Stale stamp: kick fires again.
	stale := time.Now().UTC().AddDate(0, 0, -10).Format("2006-01-02 15:04:05")
	if _, err := conn.Exec(`UPDATE metadata SET value=? WHERE key='dashboard_last_seen'`, stale); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	srv.maybeKickScoring()
	if _, ok := waitKick(); !ok {
		t.Fatal("kick did not fire for stale stamp")
	}

	// Window disabled: never kicks.
	srv.SetScoringKick(0, func(uid string) { kicked <- uid })
	srv.maybeKickScoring()
	select {
	case <-kicked:
		t.Fatal("kick fired with window disabled")
	case <-time.After(150 * time.Millisecond):
	}
}
