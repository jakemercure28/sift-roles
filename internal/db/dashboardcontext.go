package db

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// GetAppliedByCompany returns applied/responded job counts keyed by
// LOWER(TRIM(company)), mirroring getAppliedByCompany in lib/db.js.
func (r *Repository) GetAppliedByCompany() (map[string]int, error) {
	rows, err := r.query(
		"SELECT LOWER(TRIM(company)) AS co, COUNT(*) AS n FROM jobs WHERE status IN ('applied','responded') AND user_id = ? GROUP BY co",
		r.userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var co string
		var n int
		if err := rows.Scan(&co, &n); err != nil {
			return nil, err
		}
		out[co] = n
	}
	return out, rows.Err()
}

// CompanyTagsRaw returns the raw tags string per company (lowercased key) for
// companies that have tags, mirroring the companyTags load in fetchDashboardContext.
func (r *Repository) CompanyTagsRaw() (map[string]string, error) {
	rows, err := r.query(
		"SELECT company, tags FROM company_notes WHERE tags IS NOT NULL AND tags != '' AND user_id = ?",
		r.userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var company string
		var tags sql.NullString
		if err := rows.Scan(&company, &tags); err != nil {
			return nil, err
		}
		out[strings.ToLower(strings.TrimSpace(company))] = tags.String
	}
	return out, rows.Err()
}

// APIUsageToday returns today's total Gemini call count across models, for the
// sidebar API-usage card (mirrors the apiUsage.used computation).
func (r *Repository) APIUsageToday() (int, error) {
	var n int
	err := r.queryRow(
		"SELECT COALESCE(SUM(call_count), 0) FROM api_usage WHERE date = date('now','localtime') AND user_id = ?",
		r.userID,
	).Scan(&n)
	return n, err
}

func (r *Repository) ResetAPIUsageToday() error {
	_, err := r.exec("DELETE FROM api_usage WHERE date = date('now','localtime') AND user_id = ?", r.userID)
	return err
}

// CountAllJobs returns the total number of jobs (isFirstRun is count == 0).
func (r *Repository) CountAllJobs() (int, error) {
	var n int
	err := r.queryRow("SELECT COUNT(*) FROM jobs WHERE user_id = ?", r.userID).Scan(&n)
	return n, err
}

// DiscoveryReportMeta is the persisted summary of the last non-skipped company
// discovery run (metadata key 'discovery_last_report'), read by the dashboard
// scrape digest.
type DiscoveryReportMeta struct {
	At    string `json:"at"`
	Added int    `json:"added"`
}

const discoveryReportKey = "discovery_last_report"

// WriteDiscoveryReport upserts the last discovery run summary.
func (r *Repository) WriteDiscoveryReport(added int) error {
	payload, err := json.Marshal(DiscoveryReportMeta{
		At:    time.Now().UTC().Format(time.RFC3339),
		Added: added,
	})
	if err != nil {
		return err
	}
	_, err = r.exec(
		`INSERT INTO metadata (user_id, key, value, updated_at) VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		r.userID, discoveryReportKey, string(payload),
	)
	return err
}

// DiscoveryReport reads the last discovery run summary. found=false when no
// discovery run has been recorded yet.
func (r *Repository) DiscoveryReport() (DiscoveryReportMeta, bool, error) {
	var raw string
	err := r.queryRow("SELECT value FROM metadata WHERE key = ? AND user_id = ?", discoveryReportKey, r.userID).Scan(&raw)
	if err == sql.ErrNoRows {
		return DiscoveryReportMeta{}, false, nil
	}
	if err != nil {
		return DiscoveryReportMeta{}, false, err
	}
	var m DiscoveryReportMeta
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return DiscoveryReportMeta{}, false, nil
	}
	return m, true, nil
}

// ScraperHeartbeat parses the stored heartbeat JSON. found=false when no
// heartbeat has been written yet (the dashboard then renders no banner).
func (r *Repository) ScraperHeartbeat() (Heartbeat, bool, error) {
	raw, err := r.ScraperHeartbeatRaw()
	if err != nil {
		return Heartbeat{}, false, err
	}
	if raw == "" {
		return Heartbeat{}, false, nil
	}
	var hb Heartbeat
	if err := json.Unmarshal([]byte(raw), &hb); err != nil {
		return Heartbeat{}, false, nil
	}
	return hb, true, nil
}
