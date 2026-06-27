package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// RejectionSyncStatus is the persisted outcome of the last real rejection
// email sync attempt (metadata key 'rejection_sync_status'). Skipped runs
// (sync disabled, credentials missing) are not recorded; the dashboard derives
// those states from the environment instead.
type RejectionSyncStatus struct {
	LastRunAt  string `json:"last_run_at"`
	Status     string `json:"status"` // "ok" | "error"
	Error      string `json:"error,omitempty"`
	Fetched    int    `json:"fetched"`
	Candidates int    `json:"candidates"`
	Applied    int    `json:"applied"`
	Ignored    int    `json:"ignored"`
	Unmatched  int    `json:"unmatched"`
}

const rejectionSyncStatusKey = "rejection_sync_status"

// WriteRejectionSyncStatus upserts the last sync outcome, stamping LastRunAt.
func (r *Repository) WriteRejectionSyncStatus(st RejectionSyncStatus) error {
	st.LastRunAt = time.Now().UTC().Format(time.RFC3339)
	payload, err := json.Marshal(st)
	if err != nil {
		return err
	}
	_, err = r.exec(
		`INSERT INTO metadata (user_id, key, value, updated_at) VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		r.userID, rejectionSyncStatusKey, string(payload),
	)
	return err
}

// ReadRejectionSyncStatus reads the last sync outcome. found=false when no
// sync has been recorded yet.
func (r *Repository) ReadRejectionSyncStatus() (RejectionSyncStatus, bool, error) {
	var raw string
	err := r.queryRow("SELECT value FROM metadata WHERE key = ? AND user_id = ?", rejectionSyncStatusKey, r.userID).Scan(&raw)
	if err == sql.ErrNoRows {
		return RejectionSyncStatus{}, false, nil
	}
	if err != nil {
		return RejectionSyncStatus{}, false, err
	}
	var st RejectionSyncStatus
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return RejectionSyncStatus{}, false, nil
	}
	return st, true, nil
}

// CountRejectionsAppliedSince counts rejection emails auto-applied to jobs in
// the last N days, for the Settings integration status line.
func (r *Repository) CountRejectionsAppliedSince(days int) (int, error) {
	var n int
	err := r.queryRow(
		"SELECT COUNT(*) FROM rejection_email_log WHERE match_status = 'applied' AND created_at >= datetime('now', ?) AND user_id = ?",
		fmt.Sprintf("-%d days", days), r.userID,
	).Scan(&n)
	return n, err
}
