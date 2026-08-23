package db

// DistinctATSJobURLs returns the distinct apply URLs on this tenant's jobs that
// point at a public ATS board (Greenhouse, Lever, Ashby, or Workday). The URL
// harvest parses these into verified registry boards; see
// discovery.ParseATSBoardURL. The LIKE filter is dialect-portable (it rewrites to
// the same clause on SQLite and Postgres), so no regex is used here.
func (r *Repository) DistinctATSJobURLs() ([]string, error) {
	rows, err := r.query(
		`SELECT DISTINCT url FROM jobs
		 WHERE url IS NOT NULL AND url <> ''
		   AND (url LIKE '%greenhouse.io/%'
		     OR url LIKE '%lever.co/%'
		     OR url LIKE '%ashbyhq.com/%'
		     OR url LIKE '%myworkdayjobs.com/%')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
