package db

import (
	"database/sql"
	"time"
)

const (
	DescriptionCriticalLength = 50
	DescriptionWarnLength     = 300
)

// DescriptionHealthJob is one job with a suspicious description.
type DescriptionHealthJob struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Company  string `json:"company"`
	Platform string `json:"platform"`
	Len      int    `json:"len"`
}

// DescriptionHealth mirrors jd-health.json.
type DescriptionHealth struct {
	Timestamp string                 `json:"timestamp,omitempty"`
	Critical  []DescriptionHealthJob `json:"critical"`
	Warn      []DescriptionHealthJob `json:"warn"`
	Total     int                    `json:"total"`
	OK        int                    `json:"-"`
}

// CheckDescriptions checks jobs created on the given local YYYY-MM-DD date for
// missing or suspiciously short descriptions, ported from scripts/check-descriptions.js.
func (r *Repository) CheckDescriptions(localDate string) (DescriptionHealth, error) {
	rows, err := r.query(`
		SELECT id, title, company, platform, COALESCE(length(description), 0) AS len
		FROM jobs
		WHERE date(created_at, 'localtime') = ? AND user_id = ?`, localDate, r.userID)
	if err != nil {
		return DescriptionHealth{}, err
	}
	defer rows.Close()

	var health DescriptionHealth
	for rows.Next() {
		var j DescriptionHealthJob
		var title, company, platform sql.NullString
		if err := rows.Scan(&j.ID, &title, &company, &platform, &j.Len); err != nil {
			return DescriptionHealth{}, err
		}
		j.Title = title.String
		j.Company = company.String
		j.Platform = platform.String
		health.Total++
		switch {
		case j.Len < DescriptionCriticalLength:
			health.Critical = append(health.Critical, j)
		case j.Len < DescriptionWarnLength:
			health.Warn = append(health.Warn, j)
		default:
			health.OK++
		}
	}
	if err := rows.Err(); err != nil {
		return DescriptionHealth{}, err
	}
	if health.Critical == nil {
		health.Critical = []DescriptionHealthJob{}
	}
	if health.Warn == nil {
		health.Warn = []DescriptionHealthJob{}
	}
	health.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	return health, nil
}
