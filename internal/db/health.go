package db

// Ping verifies the database connection is alive. The dashboard front door uses
// it for the /healthz probe.
func (r *Repository) Ping() error {
	return r.db.Ping()
}
