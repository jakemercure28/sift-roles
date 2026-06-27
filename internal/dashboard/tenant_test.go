package dashboard

import (
	"path/filepath"
	"testing"

	"job-search-automation/internal/db"
)

func TestTenantDataDir(t *testing.T) {
	const base = "data"
	cases := []struct {
		name   string
		dt     db.DBType
		userID string
		want   string
	}{
		{"sqlite keeps root for any user", db.SQLite, "u123", base},
		{"sqlite keeps root for local", db.SQLite, db.LocalUser, base},
		{"postgres empty user keeps root", db.Postgres, "", base},
		{"postgres local keeps root", db.Postgres, db.LocalUser, base},
		{"postgres tenant is nested", db.Postgres, "u123", filepath.Join(base, "storage", "users", "u123")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tenantDataDir(base, tc.dt, tc.userID); got != tc.want {
				t.Errorf("tenantDataDir(%q, %q, %q) = %q, want %q", base, tc.dt, tc.userID, got, tc.want)
			}
		})
	}
}
