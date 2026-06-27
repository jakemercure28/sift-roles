package dashboard

import "job-search-automation/internal/db"

// tenantDataDir resolves the per-tenant profile directory for a request. It
// delegates to db.TenantDataDir so the dashboard and the one-shot migrator share
// one mapping; see that function for the SQLite/LocalUser vs hosted-Postgres
// behavior.
func tenantDataDir(base string, dt db.DBType, userID string) string {
	return db.TenantDataDir(base, dt, userID)
}
