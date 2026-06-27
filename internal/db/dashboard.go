package db

import "database/sql"

// ScraperHeartbeatRaw returns the raw JSON stored under metadata
// 'scraper_heartbeat', or "" if absent. Mirrors getScraperHeartbeat in
// lib/dashboard-insights.js (the dashboard emits null when this is empty).
func (r *Repository) ScraperHeartbeatRaw() (string, error) {
	var raw sql.NullString
	err := r.queryRow(
		"SELECT value FROM metadata WHERE key = 'scraper_heartbeat' AND user_id = ?", r.userID,
	).Scan(&raw)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return raw.String, nil
}

// ScoringStats holds the raw counts behind /api/scoring-progress. The ETA,
// active, and quota-exhausted derivations are computed by the handler so they
// can use the configured rate delay and daily limit.
type ScoringStats struct {
	Scored           int
	Unscored         int
	LatestScoreAt    string
	NewJobs24h       int
	NewCompanies24h  int
	StrongFitsNew24h int
	APIUsedToday     int
	RecentQuotaError bool
}

// ScoringStats gathers the counts in handleScoringProgress (lib/dashboard-routes.js).
func (r *Repository) ScoringStats() (ScoringStats, error) {
	var s ScoringStats
	var latest sql.NullString
	if err := r.queryRow(`
		SELECT
			SUM(CASE WHEN score IS NOT NULL THEN 1 ELSE 0 END),
			SUM(CASE WHEN score IS NULL AND status = 'pending' THEN 1 ELSE 0 END),
			MAX(CASE WHEN score IS NOT NULL THEN updated_at END)
		FROM jobs WHERE user_id = ?`, r.userID).Scan(&nullInt{&s.Scored}, &nullInt{&s.Unscored}, &latest); err != nil {
		return s, err
	}
	s.LatestScoreAt = latest.String

	if err := r.queryRow(`
		SELECT
			(SELECT COUNT(*) FROM jobs WHERE created_at >= datetime('now','-24 hours') AND user_id = ?),
			(SELECT COUNT(*) FROM (
				SELECT company FROM jobs WHERE user_id = ? GROUP BY company
				HAVING MIN(created_at) >= datetime('now','-24 hours')
			)),
			(SELECT COUNT(*) FROM jobs WHERE created_at >= datetime('now','-24 hours') AND score >= 7 AND user_id = ?)`,
		r.userID, r.userID, r.userID).
		Scan(&s.NewJobs24h, &s.NewCompanies24h, &s.StrongFitsNew24h); err != nil {
		return s, err
	}

	if err := r.queryRow(`
		SELECT COALESCE(SUM(call_count), 0)
		FROM api_usage WHERE date = date('now','localtime') AND user_id = ?`, r.userID).Scan(&s.APIUsedToday); err != nil {
		return s, err
	}

	var dummy int
	err := r.queryRow(`
		SELECT 1 FROM jobs
		WHERE score IS NULL
			AND score_error IS NOT NULL
			AND (score_error LIKE '%exceeded your current quota%' OR score_error LIKE '%RESOURCE_EXHAUSTED%')
			AND last_score_attempt_at > datetime('now', '-15 minutes')
			AND user_id = ?
		LIMIT 1`, r.userID).Scan(&dummy)
	if err == nil {
		s.RecentQuotaError = true
	} else if err != sql.ErrNoRows {
		return s, err
	}
	return s, nil
}

// JobDescription is the payload of GET /job-description.
type JobDescription struct {
	Title       string `json:"title"`
	Company     string `json:"company"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

// JobDescription returns a job's display fields, or found=false if missing.
func (r *Repository) JobDescription(id string) (JobDescription, bool, error) {
	var d JobDescription
	var title, company, url, desc sql.NullString
	err := r.queryRow(
		"SELECT title, company, url, description FROM jobs WHERE id = ? AND user_id = ?", id, r.userID,
	).Scan(&title, &company, &url, &desc)
	if err == sql.ErrNoRows {
		return d, false, nil
	}
	if err != nil {
		return d, false, err
	}
	d.Title, d.Company, d.URL, d.Description = title.String, company.String, url.String, desc.String
	return d, true, nil
}

// CompanyNotes returns the stored tags/notes for a company key (already
// lowercased/trimmed by the caller). found=false when no row exists.
func (r *Repository) CompanyNotes(company string) (tags, notes string, found bool, err error) {
	var t, n sql.NullString
	err = r.queryRow(
		"SELECT tags, notes FROM company_notes WHERE company = ? AND user_id = ?", company, r.userID,
	).Scan(&t, &n)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return t.String, n.String, true, nil
}

// SaveCompanyNotes upserts a company's tags/notes, mirroring handleSaveCompanyNotes.
func (r *Repository) SaveCompanyNotes(company, tags, notes string) error {
	_, err := r.exec(`
		INSERT INTO company_notes (user_id, company, tags, notes, updated_at)
		VALUES (?, ?, ?, ?, datetime('now'))
		ON CONFLICT(user_id, company) DO UPDATE SET tags=excluded.tags, notes=excluded.notes, updated_at=datetime('now')`,
		r.userID, company, tags, notes)
	return err
}

// ArchiveJob sets a job to archived and logs the status change, mirroring
// handleArchive (transactional touch + event).
func (r *Repository) ArchiveJob(id string) error {
	return r.inTx(func(ex execer) error {
		if _, err := ex.Exec(
			"UPDATE jobs SET status='archived', updated_at=datetime('now') WHERE id=? AND user_id=?", id, r.userID,
		); err != nil {
			return err
		}
		return logEventTx(ex, r.userID, id, "status_change", "", "archived")
	})
}

// SetPipelineStage applies a pipeline stage transition and logs the stage change,
// mirroring the stage machine in handlePipeline. value must be pre-validated.
func (r *Repository) SetPipelineStage(id, value string) error {
	return r.inTx(func(ex execer) error {
		var fromStage sql.NullString
		if err := ex.QueryRow("SELECT stage FROM jobs WHERE id=? AND user_id=?", id, r.userID).Scan(&fromStage); err != nil {
			return err
		}

		var exec string
		var args []any
		to := value
		switch value {
		case "":
			exec = "UPDATE jobs SET status='pending', stage=NULL, updated_at=datetime('now') WHERE id=? AND user_id=?"
			args = []any{id, r.userID}
			to = ""
		case "closed":
			exec = "UPDATE jobs SET status='closed', stage='closed', updated_at=datetime('now') WHERE id=? AND user_id=?"
			args = []any{id, r.userID}
		case "rejected":
			exec = "UPDATE jobs SET status='rejected', stage='rejected', rejected_from_stage=?, rejected_at=datetime('now'), updated_at=datetime('now') WHERE id=? AND user_id=?"
			args = []any{fromStage, id, r.userID}
		case "ghosted":
			exec = "UPDATE jobs SET status='ghosted', stage='ghosted', updated_at=datetime('now') WHERE id=? AND user_id=?"
			args = []any{id, r.userID}
		default: // applied, phone_screen, interview, onsite, offer
			exec = "UPDATE jobs SET status='applied', stage=?, applied_at=COALESCE(applied_at, datetime('now')), updated_at=datetime('now') WHERE id=? AND user_id=?"
			args = []any{value, id, r.userID}
		}

		if _, err := ex.Exec(exec, args...); err != nil {
			return err
		}
		return logEventTx(ex, r.userID, id, "stage_change", fromStage.String, to)
	})
}

// JobBrief is the subset needed to score rejection likelihood after a pipeline
// transition.
type JobBrief struct {
	Title       string
	Company     string
	Location    string
	Description string
}

// JobBrief loads a job's scoring fields, or found=false if missing.
func (r *Repository) JobBrief(id string) (JobBrief, bool, error) {
	var j JobBrief
	var title, company, location, desc sql.NullString
	err := r.queryRow(
		"SELECT title, company, location, description FROM jobs WHERE id = ? AND user_id = ?", id, r.userID,
	).Scan(&title, &company, &location, &desc)
	if err == sql.ErrNoRows {
		return j, false, nil
	}
	if err != nil {
		return j, false, err
	}
	j.Title, j.Company, j.Location, j.Description = title.String, company.String, location.String, desc.String
	return j, true, nil
}

// SetRejectionReasoning stores the background rejection-likelihood text.
func (r *Repository) SetRejectionReasoning(id, text string) error {
	_, err := r.exec(
		"UPDATE jobs SET rejection_reasoning=?, updated_at=datetime('now') WHERE id=? AND user_id=?", text, id, r.userID,
	)
	return err
}

// inTx runs fn inside a transaction, committing on success and rolling back on
// error.
func (r *Repository) inTx(fn func(execer) error) error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	// Under RLS, stamp the transaction-local app.user_id GUC the policies key on,
	// the same way the standalone exec/query/queryRow helpers do.
	if r.rls {
		if _, err := tx.Exec(setRLSUserSQL, r.userID); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := fn(execer{raw: tx, dl: r.dl}); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// logEventTx writes an events row inside a transaction (empty from/to -> NULL),
// matching LogEvent / logEvent in lib/db.js. It takes an sqlExec so it works on a
// rewrite-aware execer or a raw *sql.Tx.
func logEventTx(ex sqlExec, userID, jobID, eventType, from, to string) error {
	_, err := ex.Exec(
		"INSERT INTO events (user_id, job_id, event_type, from_value, to_value) VALUES (?, ?, ?, ?, ?)",
		userID, jobID, eventType, nullable(from), nullable(to),
	)
	return err
}

// nullInt scans a possibly-NULL integer aggregate into *int (NULL -> 0).
type nullInt struct{ dst *int }

func (n *nullInt) Scan(src any) error {
	var ni sql.NullInt64
	if err := ni.Scan(src); err != nil {
		return err
	}
	*n.dst = int(ni.Int64)
	return nil
}
