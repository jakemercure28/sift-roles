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
// matching scripts/check-closed.js. Applied/responded rows are excluded: once a
// job is in the human-controlled pipeline (applied through offer all carry
// status='applied'), a public posting coming down must not auto-close the row
// out from under an in-flight application or interview. The probe only exists to
// declutter unapplied listings from the review queue.
func (r *Repository) CloseCheckJobs() ([]CloseCheckJob, error) {
	rows, err := r.query(`
		SELECT id, company, title, platform, url, stage
		FROM jobs
		WHERE status NOT IN ('archived', 'applied', 'responded')
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
