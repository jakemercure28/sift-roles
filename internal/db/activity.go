package db

import (
	"database/sql"
	"time"
)

// dashboardLastSeenKey is the metadata key holding a tenant's most recent
// dashboard visit (UTC "YYYY-MM-DD HH:MM:SS"). One row per user (metadata's PK
// is (user_id, key)), so it is a last-write-wins "last seen" stamp, not a log.
const dashboardLastSeenKey = "dashboard_last_seen"

// TouchLastSeen records that this tenant just loaded their dashboard. It is the
// engagement signal the events table cannot carry: events require a job_id (FK,
// NOT NULL), but a visit is not tied to a job. Upsert keeps exactly one row per
// tenant. Callers treat failures as non-fatal: a missed stamp only understates
// activity, it never breaks a page load.
func (r *Repository) TouchLastSeen() error {
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	_, err := r.exec(
		`INSERT INTO metadata (user_id, key, value, updated_at) VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		r.userID, dashboardLastSeenKey, now)
	return err
}

// DashboardLastSeen returns this tenant's dashboard_last_seen stamp (UTC
// "YYYY-MM-DD HH:MM:SS"), or "" when the tenant has never loaded the dashboard
// since the stamp shipped. Fixed-width layout, so callers may compare stamps
// lexically like CountActiveUsers does.
func (r *Repository) DashboardLastSeen() (string, error) {
	var v string
	err := r.queryRow(
		`SELECT value FROM metadata WHERE user_id = ? AND key = ?`,
		r.userID, dashboardLastSeenKey).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// CountActiveUsers returns how many distinct tenants have a dashboard_last_seen
// at or after sinceUTC (same "YYYY-MM-DD HH:MM:SS" UTC format TouchLastSeen
// writes; lexical compare is safe for that fixed-width layout). It is a global
// read across tenants, so it runs on the base repository from the metrics loop,
// not a tenant-scoped clone.
func (r *Repository) CountActiveUsers(sinceUTC string) (int, error) {
	var n int
	err := r.queryRow(
		`SELECT COUNT(*) FROM metadata WHERE key = ? AND value >= ?`,
		dashboardLastSeenKey, sinceUTC).Scan(&n)
	return n, err
}
