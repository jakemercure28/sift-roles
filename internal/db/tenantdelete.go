package db

import "fmt"

// tenantTables lists every table keyed or filtered by user_id. DeleteTenant wipes
// all of a tenant's rows across them. Keep in sync with the schema baseline
// (migrations/postgres/00001_baseline.sql, migrations/sqlite/00001_baseline.sql).
var tenantTables = []string{
	"jobs",
	"metadata",
	"api_usage",
	"company_notes",
	"events",
	"rejection_email_log",
	"job_aliases",
	"status_snapshots",
	"ats_resolution_cache",
}

// DeleteTenant permanently deletes every row owned by this repo's tenant across
// all user_id-scoped tables, in one transaction (all or nothing). It does NOT
// remove the tenant's on-disk profile directory (resume.md, context.md, etc.);
// the caller is responsible for that, since the repo does not own the filesystem
// layout. Returns the total number of rows deleted.
//
// This is the data-deletion primitive behind on-demand account deletion and the
// inactivity purge. It refuses to run for an empty user id as a guard against
// wiping an unscoped repo.
func (r *Repository) DeleteTenant() (int64, error) {
	if r.userID == "" {
		return 0, fmt.Errorf("DeleteTenant: refusing to delete with an empty user id")
	}
	tx, err := r.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Defense-in-depth: when RLS is enforced, scope the transaction to this tenant
	// the same way r.exec does, so the deletes run under the restricted role.
	if r.rls {
		if _, err := tx.Exec(setRLSUserSQL, r.userID); err != nil {
			return 0, err
		}
	}
	var total int64
	for _, t := range tenantTables {
		res, err := tx.Exec(r.dl.rewrite("DELETE FROM "+t+" WHERE user_id = ?"), r.userID)
		if err != nil {
			return 0, fmt.Errorf("delete from %s: %w", t, err)
		}
		if n, aerr := res.RowsAffected(); aerr == nil {
			total += n
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}
