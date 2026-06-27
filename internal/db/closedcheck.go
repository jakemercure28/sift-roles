package db

import "database/sql"

// CloseCheckJob is the row shape needed by the closed-job checker.
type CloseCheckJob struct {
	ID       string
	Company  string
	Title    string
	Platform string
	URL      string
	Stage    string
}

// CloseCheckJobs returns active rows eligible for source-ATS closure probing,
// matching scripts/check-closed.js.
func (r *Repository) CloseCheckJobs() ([]CloseCheckJob, error) {
	rows, err := r.query(`
		SELECT id, company, title, platform, url, stage
		FROM jobs
		WHERE status != 'archived'
			AND (stage IS NULL OR stage NOT IN ('closed', 'rejected', 'offer'))
			AND user_id = ?
		ORDER BY platform, company`, r.userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CloseCheckJob
	for rows.Next() {
		var j CloseCheckJob
		var company, title, platform, url, stage sql.NullString
		if err := rows.Scan(&j.ID, &company, &title, &platform, &url, &stage); err != nil {
			return nil, err
		}
		j.Company = company.String
		j.Title = title.String
		j.Platform = platform.String
		j.URL = url.String
		j.Stage = stage.String
		out = append(out, j)
	}
	return out, rows.Err()
}
