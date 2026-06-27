package db

// Per-tenant metadata helpers backed by the metadata table (keyed by user_id),
// so there is no events FK to satisfy.

// SetMetadataOnce writes value under key for the scoped tenant only if nothing is
// stored there yet, preserving the FIRST occurrence. It is a no-op when a value
// already exists, so it is safe to call on every poll / save.
func (r *Repository) SetMetadataOnce(key, value string) error {
	_, err := r.exec(
		`INSERT INTO metadata (user_id, key, value, updated_at) VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT(user_id, key) DO NOTHING`,
		r.userID, key, value,
	)
	return err
}
