package db

import "testing"

// TestDeleteTenantWipesOnlyThatTenant seeds two tenants across several user_id
// tables, deletes one, and verifies the deleted tenant's rows are gone everywhere
// while the other tenant's data is untouched.
func TestDeleteTenantWipesOnlyThatTenant(t *testing.T) {
	base := newTestRepo(t)
	a := base.ForUser("user-a")
	b := base.ForUser("user-b")

	// Both tenants get a job. Only user-b gets metadata + api_usage rows: the legacy
	// SQLite test schema makes metadata.key and api_usage(date,model) globally
	// unique (prod Postgres folds user_id into both keys), so two tenants can't hold
	// the same key/usage row in the test DB. Seeding them on the deleted tenant
	// still exercises those tables' delete branches; user-a's surviving job proves
	// isolation.
	for _, r := range []*Repository{a, b} {
		if _, err := r.InsertScrapedLead(sampleLead("job-" + r.userID)); err != nil {
			t.Fatalf("insert lead for %s: %v", r.userID, err)
		}
	}
	if err := b.SetMetadataOnce("seed_marker", "user-b"); err != nil {
		t.Fatalf("set metadata for user-b: %v", err)
	}
	if err := b.RecordAPICall("test-model"); err != nil {
		t.Fatalf("record api call for user-b: %v", err)
	}

	count := func(table, uid string) int {
		t.Helper()
		var n int
		if err := base.db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE user_id = ?", uid).Scan(&n); err != nil {
			t.Fatalf("count %s for %s: %v", table, uid, err)
		}
		return n
	}

	if got, err := b.DeleteTenant(); err != nil || got == 0 {
		t.Fatalf("DeleteTenant(user-b) = %d, %v; want >0 rows, nil", got, err)
	}

	// user-b is wiped across every tenant table.
	for _, table := range tenantTables {
		if n := count(table, "user-b"); n != 0 {
			t.Fatalf("user-b still has %d rows in %s after delete", n, table)
		}
	}
	// user-a is untouched.
	if n := count("jobs", "user-a"); n != 1 {
		t.Fatalf("user-a jobs = %d, want 1 (must survive user-b delete)", n)
	}
}

// TestDeleteTenantRefusesEmptyUser guards against wiping an unscoped repo.
func TestDeleteTenantRefusesEmptyUser(t *testing.T) {
	repo := newTestRepo(t)
	empty := repo.ForUser("")
	if _, err := empty.DeleteTenant(); err == nil {
		t.Fatal("DeleteTenant with empty user id should error")
	}
}
