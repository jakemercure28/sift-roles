package db

import (
	"database/sql"
	"strings"

	"job-search-automation/internal/model"
)

// This file owns steady-state distribution: after the global board pass
// harvests the cross-tenant board union into global_jobs, each tenant's queue
// is fed from that cache instead of by scraping the same boards again per
// tenant. The SQL here is a broad prefilter (company watchlist OR title LIKE);
// the caller applies the exact worker-parity term matcher before inserting.

// DistributionLead is a global row candidate for one tenant, carrying the
// last_seen_at cursor value so the caller can advance its per-tenant cursor.
type DistributionLead struct {
	Lead       model.Lead
	LastSeenAt string
}

// GlobalLeadsForDistribution returns global rows seen after cursor that either
// belong to one of the tenant's watchlist companies (case-insensitive) or have
// a title LIKE-matching one of the tenant's search terms. Rows are ordered by
// last_seen_at ascending so the caller can advance its cursor monotonically;
// limit bounds one pass. Descriptionless rows are skipped (they would only be
// resolved deterministically by scoring anyway).
func (r *Repository) GlobalLeadsForDistribution(cursor string, companies, terms []string, limit int) ([]DistributionLead, error) {
	return r.globalLeads(cursor, "", companies, terms, limit, false)
}

// GlobalLeadsBackfill returns matching rows in the half-open window
// (since, until], newest first. distributeTenant uses it to recover rows
// stranded just below its cursor: within one scrape pass, boards are written to
// global_jobs at different times, so a row scraped early in the pass (a lower
// last_seen_at) is orphaned when the tenant's cursor jumps to a row scraped
// later in the same pass. The forward query only looks past the cursor and never
// revisits it, so those rows would never distribute. This pass sweeps a grace
// window below the cursor newest-first (the newest orphans sit right under the
// watermark, so DESC finds them before the limit bites) without advancing the
// cursor; InsertDistributedLead dedups, so re-offered rows are no-ops.
func (r *Repository) GlobalLeadsBackfill(since, until string, companies, terms []string, limit int) ([]DistributionLead, error) {
	if until == "" {
		return nil, nil
	}
	return r.globalLeads(since, until, companies, terms, limit, true)
}

// globalLeads is the shared query behind both the forward distribution pass and
// the grace-window backfill: matching, described rows in the half-open window
// (since, until], ordered by last_seen_at ascending (forward, so the cursor
// advances monotonically) or descending (backfill, so the newest orphans are
// caught first). Either bound may be empty to leave that side open.
func (r *Repository) globalLeads(since, until string, companies, terms []string, limit int, desc bool) ([]DistributionLead, error) {
	if limit <= 0 || (len(companies) == 0 && len(terms) == 0) {
		return nil, nil
	}
	var match []string
	args := []any{}
	if since != "" {
		args = append(args, since)
	}
	if until != "" {
		args = append(args, until)
	}
	if len(companies) > 0 {
		ph := make([]string, len(companies))
		for i, c := range companies {
			ph[i] = "?"
			args = append(args, strings.ToLower(strings.TrimSpace(c)))
		}
		match = append(match, "LOWER(company) IN ("+strings.Join(ph, ",")+")")
	}
	for _, t := range terms {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		match = append(match, "LOWER(title) LIKE ?")
		args = append(args, "%"+t+"%")
	}
	if len(match) == 0 {
		return nil, nil
	}
	q := `
		SELECT id, title, company, url, platform, location, posted_at, scraped_at, description, last_seen_at
		FROM global_jobs
		WHERE COALESCE(description, '') <> ''`
	if since != "" {
		q += " AND last_seen_at > ?"
	}
	if until != "" {
		q += " AND last_seen_at <= ?"
	}
	order := "ASC"
	if desc {
		order = "DESC"
	}
	q += " AND (" + strings.Join(match, " OR ") + `)
		ORDER BY last_seen_at ` + order + " LIMIT ?"
	args = append(args, limit)

	rows, err := r.query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DistributionLead
	for rows.Next() {
		var d DistributionLead
		var title, company, url, platform, location, postedAt, scrapedAt, description, lastSeen sql.NullString
		if err := rows.Scan(&d.Lead.ID, &title, &company, &url, &platform, &location, &postedAt, &scrapedAt, &description, &lastSeen); err != nil {
			return nil, err
		}
		d.Lead.Title = title.String
		d.Lead.Company = company.String
		d.Lead.DirectApplyURL = url.String
		d.Lead.ATSPlatformName = platform.String
		d.Lead.Location = location.String
		d.Lead.PostedAt = postedAt.String
		d.Lead.ScrapedTimestamp = scrapedAt.String
		d.Lead.Description = description.String
		d.LastSeenAt = lastSeen.String
		out = append(out, d)
	}
	return out, rows.Err()
}

// InsertDistributedLead inserts one distribution candidate into the tenant's
// queue as pending/unscored, idempotent per tenant via (user_id,
// global_job_id). It reuses the scraped-lead insert path so distributed rows
// are indistinguishable from directly scraped ones downstream.
func (r *Repository) InsertDistributedLead(lead model.Lead) (bool, error) {
	return r.insertScrapedLead(lead, false)
}

// VanishedBoardJob is one active tenant row whose posting disappeared from
// its board, keyed by the lowercase company the scheduler groups closes on.
type VanishedBoardJob struct {
	ID      string
	Company string
	// GlobalID/URL identify the posting itself, so the pre-close liveness
	// probe can ask its board directly and stamp a verified sighting.
	GlobalID string
	URL      string
}

// VanishedBoardJobs returns this tenant's active jobs whose global row
// belongs to one of the given (platform, company) boards but was NOT seen
// since cutoff, i.e. the posting disappeared from its board. The caller
// passes a grace-shifted cutoff (pass start minus the close grace window) so
// a posting must stay missing for the whole window before it closes: Workday
// boards flap postings out of one listing and back into the next (nike/usc,
// 2026-07-05), and a single missed cycle proves nothing. A row also survives
// when ANY global row with the same URL was seen within the window, compared
// case-insensitively: scraper id derivation has drifted before (Workday,
// 2026-06-24), and board-path casing has drifted too (registry-harvested
// lowercase entries replacing labeled ones, /en-US/Jobs/ vs /en-US/jobs/,
// 2026-07-05), both leaving live postings stranded under rows a re-harvest
// never touches. The URL, case-folded, is the scheme-independent identity of
// the posting. Eligibility mirrors CloseCheckJobs so the diff never touches
// archived/applied/responded/closed/rejected/offer rows (applied-pipeline rows
// are the user's to manage, never auto-closed). companiesLower must be lowercased.
// The caller guarantees each named board was actually fetched completely
// this run (a failed or partial fetch must not read as "everything vanished").
func (r *Repository) VanishedBoardJobs(platform string, companiesLower []string, cutoff string) ([]VanishedBoardJob, error) {
	if platform == "" || len(companiesLower) == 0 || cutoff == "" {
		return nil, nil
	}
	ph := make([]string, len(companiesLower))
	args := []any{r.userID, platform}
	for i, c := range companiesLower {
		ph[i] = "?"
		args = append(args, c)
	}
	args = append(args, cutoff, cutoff)
	rows, err := r.query(`
		SELECT j.id, LOWER(g.company), g.id, g.url
		FROM jobs j
		JOIN global_jobs g ON g.id = j.global_job_id
		WHERE j.user_id = ?
		  AND j.status NOT IN ('archived', 'applied', 'responded')
		  AND (j.stage IS NULL OR j.stage NOT IN ('closed', 'rejected', 'offer'))
		  AND LOWER(g.platform) = ?
		  AND LOWER(g.company) IN (`+strings.Join(ph, ",")+`)
		  AND g.last_seen_at < ?
		  AND NOT EXISTS (
		    SELECT 1 FROM global_jobs g2
		    WHERE LOWER(g2.url) = LOWER(g.url) AND g2.last_seen_at >= ?
		  )`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vanished []VanishedBoardJob
	for rows.Next() {
		var v VanishedBoardJob
		if err := rows.Scan(&v.ID, &v.Company, &v.GlobalID, &v.URL); err != nil {
			return nil, err
		}
		vanished = append(vanished, v)
	}
	return vanished, rows.Err()
}

// TouchGlobalJobSeen stamps a verified live sighting on a global row: the
// diff's pre-close probe found the posting on its board even though the
// listing fetch missed it, so the grace window and the reopen pass must treat
// it as seen. Without the stamp the row stays stale and every later cycle
// re-candidates it for closing.
func (r *Repository) TouchGlobalJobSeen(globalID string) error {
	if globalID == "" {
		return nil
	}
	_, err := r.exec(`UPDATE global_jobs SET last_seen_at = datetime('now') WHERE id = ?`, globalID)
	return err
}

// ResurfacedClosedJobs returns this tenant's diff/probe-closed rows whose
// posting was seen on its board again since seenSince: a fresh sighting of
// the same URL (case-insensitive, matching the VanishedBoardJobs survival
// check) proves the posting is live, so the close was wrong or the posting
// was reposted. Only automation-closed rows qualify (status AND stage both
// 'closed'); rejected, archived, ghosted, and applied-pipeline rows are the
// user's to manage. This is the self-healing half of the closed-by-diff pass:
// Workday boards flap live postings out of individual listings (observed
// 2026-07-05), and without a reopen path a flap-closed job stayed closed
// forever because the distribution insert is idempotent per global row.
func (r *Repository) ResurfacedClosedJobs(seenSince string) ([]string, error) {
	if seenSince == "" {
		return nil, nil
	}
	rows, err := r.query(`
		SELECT j.id
		FROM jobs j
		JOIN global_jobs g ON g.id = j.global_job_id
		WHERE j.user_id = ?
		  AND j.status = 'closed'
		  AND j.stage = 'closed'
		  AND EXISTS (
		    SELECT 1 FROM global_jobs g2
		    WHERE LOWER(g2.url) = LOWER(g.url) AND g2.last_seen_at >= ?
		  )`, r.userID, seenSince)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
