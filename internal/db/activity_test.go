package db

import (
	"os"
	"testing"
	"time"
)

// TestActiveUsersWindowExcludesStale checks the window cutoff on SQLite with a
// single tenant (self-host SQLite is single-tenant: metadata.key is still a sole
// PRIMARY KEY there, so multi-tenant counting is a Postgres-only concern, covered
// by TestActiveUsersMultiTenantPostgres).
func TestActiveUsersWindowExcludesStale(t *testing.T) {
	repo := newTestRepo(t)

	// Seed a visit 10 days ago for the lone local tenant.
	stale := time.Now().UTC().AddDate(0, 0, -10).Format("2006-01-02 15:04:05")
	mustExec(t, repo.db,
		`INSERT INTO metadata (user_id, key, value, updated_at) VALUES (?, ?, ?, ?)`,
		LocalUser, dashboardLastSeenKey, stale, stale)

	since7 := time.Now().UTC().AddDate(0, 0, -7).Format("2006-01-02 15:04:05")
	if n, err := repo.CountActiveUsers(since7); err != nil {
		t.Fatalf("count 7d: %v", err)
	} else if n != 0 {
		t.Fatalf("7d active = %d, want 0 (a 10-day-old visit is outside the window)", n)
	}

	since30 := time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02 15:04:05")
	if n, err := repo.CountActiveUsers(since30); err != nil {
		t.Fatalf("count 30d: %v", err)
	} else if n != 1 {
		t.Fatalf("30d active = %d, want 1", n)
	}
}

func TestTouchLastSeenIsIdempotentPerTenant(t *testing.T) {
	repo := newTestRepo(t)
	for i := 0; i < 3; i++ {
		if err := repo.TouchLastSeen(); err != nil {
			t.Fatalf("touch %d: %v", i, err)
		}
	}
	// metadata's (user_id, key) upsert keeps exactly one row, so repeated visits
	// count the tenant once, not once per visit.
	since := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02 15:04:05")
	if n, err := repo.CountActiveUsers(since); err != nil {
		t.Fatalf("count: %v", err)
	} else if n != 1 {
		t.Fatalf("active = %d, want 1", n)
	}
}

// TestActiveUsersMultiTenantPostgres is the real prod path: many tenants share
// one Postgres DB, and only those seen within the window are counted. Gated on
// JSA_PG_DSN like the repo's other Postgres tests.
func TestActiveUsersMultiTenantPostgres(t *testing.T) {
	dsn := os.Getenv("JSA_PG_DSN")
	if dsn == "" {
		t.Skip("set JSA_PG_DSN to a throwaway Postgres DSN to run the multi-tenant active-users test")
	}
	admin, err := OpenPostgres(dsn, DefaultPoolConfig(), "", false)
	if err != nil {
		t.Fatalf("OpenPostgres admin: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	// Deterministic reruns: clear any prior last-seen rows.
	if _, err := admin.exec(`DELETE FROM metadata WHERE key = ?`, dashboardLastSeenKey); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	for _, uid := range []string{"pg-live-1", "pg-live-2"} {
		if err := admin.ForUser(uid).TouchLastSeen(); err != nil {
			t.Fatalf("touch %s: %v", uid, err)
		}
	}
	// A third tenant last visited 10 days ago.
	if err := admin.ForUser("pg-stale").TouchLastSeen(); err != nil {
		t.Fatalf("touch stale: %v", err)
	}
	stale := time.Now().UTC().AddDate(0, 0, -10).Format("2006-01-02 15:04:05")
	if _, err := admin.ForUser("pg-stale").exec(
		`UPDATE metadata SET value = ? WHERE user_id = ? AND key = ?`,
		stale, "pg-stale", dashboardLastSeenKey); err != nil {
		t.Fatalf("age stale: %v", err)
	}

	since7 := time.Now().UTC().AddDate(0, 0, -7).Format("2006-01-02 15:04:05")
	if n, err := admin.CountActiveUsers(since7); err != nil {
		t.Fatalf("count 7d: %v", err)
	} else if n != 2 {
		t.Fatalf("7d active = %d, want 2 (stale tenant excluded)", n)
	}
	since30 := time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02 15:04:05")
	if n, err := admin.CountActiveUsers(since30); err != nil {
		t.Fatalf("count 30d: %v", err)
	} else if n != 3 {
		t.Fatalf("30d active = %d, want 3", n)
	}
}

func TestDashboardLastSeen(t *testing.T) {
	repo := newTestRepo(t)

	// Never seen: empty string, no error.
	if v, err := repo.DashboardLastSeen(); err != nil || v != "" {
		t.Fatalf("unseen = (%q, %v), want (\"\", nil)", v, err)
	}

	if err := repo.TouchLastSeen(); err != nil {
		t.Fatalf("touch: %v", err)
	}
	v, err := repo.DashboardLastSeen()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02 15:04:05")
	if v < cutoff {
		t.Fatalf("stamp %q older than a day; want fresh", v)
	}
}
