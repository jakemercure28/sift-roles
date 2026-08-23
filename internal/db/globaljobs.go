package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"time"

	"job-search-automation/internal/model"
)

// JobRowID returns the tenant-local jobs.id used for a global scraper job id.
// Postgres keeps jobs.id globally unique, so hosted tenant copies of the same
// market job need distinct row ids. The original scraper id is stored in
// jobs.global_job_id and global_jobs.id.
func (r *Repository) JobRowID(globalID string) string {
	globalID = strings.TrimSpace(globalID)
	if r.DBType() != Postgres || r.userID == "" || r.userID == LocalUser {
		return globalID
	}
	sum := sha256.Sum256([]byte(r.userID + "|" + globalID))
	return globalID + "-u-" + hex.EncodeToString(sum[:])[:12]
}

// DescriptionHash is the canonical hash of a global job description, the value
// stored in global_jobs.description_hash and matched by global_job_extracts to
// detect stale extracts. Exported so tests and callers registering extracts can
// compute a hash that agrees with harvest.
func DescriptionHash(description string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(description))
	return hex.EncodeToString(sum[:])
}

func shouldHarvestGlobalLead(lead model.Lead) bool {
	if strings.TrimSpace(lead.ID) == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(lead.ATSPlatformName)) {
	case "linkedin", "manual":
		return false
	default:
		return strings.TrimSpace(lead.DirectApplyURL) != ""
	}
}

// HarvestGlobalLead upserts a scraped public listing into the cross-tenant job
// cache. Tenant-specific score/status/application state remains in jobs.
func (r *Repository) HarvestGlobalLead(lead model.Lead) error {
	if !shouldHarvestGlobalLead(lead) {
		return nil
	}
	_, err := r.exec(`
		INSERT INTO global_jobs
		  (id, title, company, url, platform, location, posted_at, scraped_at,
		   description, description_hash, first_seen_by, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))
		ON CONFLICT(id) DO UPDATE SET
		  title = COALESCE(NULLIF(excluded.title, ''), global_jobs.title),
		  company = COALESCE(NULLIF(excluded.company, ''), global_jobs.company),
		  url = COALESCE(NULLIF(excluded.url, ''), global_jobs.url),
		  platform = COALESCE(NULLIF(excluded.platform, ''), global_jobs.platform),
		  location = COALESCE(NULLIF(excluded.location, ''), global_jobs.location),
		  posted_at = COALESCE(NULLIF(excluded.posted_at, ''), global_jobs.posted_at),
		  scraped_at = COALESCE(NULLIF(excluded.scraped_at, ''), global_jobs.scraped_at),
		  description = CASE
		    WHEN COALESCE(length(excluded.description), 0) >= COALESCE(length(global_jobs.description), 0)
		    THEN excluded.description
		    ELSE global_jobs.description
		  END,
		  description_hash = CASE
		    WHEN COALESCE(length(excluded.description), 0) >= COALESCE(length(global_jobs.description), 0)
		    THEN excluded.description_hash
		    ELSE global_jobs.description_hash
		  END,
		  last_seen_at = datetime('now')`,
		lead.ID,
		lead.Title,
		lead.Company,
		lead.DirectApplyURL,
		lead.ATSPlatformName,
		lead.Location,
		normalizePostedAt(lead.PostedAt, time.Now().UTC()),
		lead.ScrapedTimestamp,
		lead.Description,
		DescriptionHash(lead.Description),
		r.userID,
	)
	return err
}

// SeedTenantJobsFromGlobal copies recent global job descriptions into this
// tenant's jobs queue as pending/unscored rows. It is idempotent per tenant via
// (user_id, global_job_id), and bounded so a first run does not flood the tenant.
//
// titleKeywords are the tenant's target-title/industry tokens. When present, the
// seed prefers global jobs whose title matches one of them so a marketing tenant
// is seeded with marketing roles instead of the most-recent rows of whatever was
// last scraped (which would hand them infra jobs and burn scoring quota burying
// them). Matched rows are ranked by how many keywords hit, then recency, so the
// onboarding burst scores the likeliest fits first. It falls back to pure recency
// only when no keyword matches anything, so a brand-new category with no
// inventory yet still gets a non-empty seed.
//
// maxAgeDays keeps the seed to listings seen within that window (0 disables):
// seeded jobs are the first thing a new tenant sees scored, so they should be
// plausibly still open. The passes relax in order: keywords within the window,
// keywords any age, recency.
func (r *Repository) SeedTenantJobsFromGlobal(limit int, titleKeywords []string, maxAgeDays int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	if kw := normalizeSeedKeywords(titleKeywords); len(kw) > 0 {
		inserted, scanned, err := r.seedFromGlobal(limit, kw, maxAgeDays)
		if err != nil || scanned > 0 {
			return inserted, err
		}
		if maxAgeDays > 0 {
			inserted, scanned, err = r.seedFromGlobal(limit, kw, 0)
			if err != nil || scanned > 0 {
				return inserted, err
			}
		}
	}
	inserted, _, err := r.seedFromGlobal(limit, nil, 0)
	return inserted, err
}

// normalizeSeedKeywords lowercases, trims, drops empties, and dedupes the raw
// keyword slice so the caller can pass profile tokens without pre-cleaning.
func normalizeSeedKeywords(keywords []string) []string {
	seen := make(map[string]bool, len(keywords))
	out := make([]string, 0, len(keywords))
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw == "" || seen[kw] {
			continue
		}
		seen[kw] = true
		out = append(out, kw)
	}
	return out
}

// seedFromGlobal runs one seed pass. When keywords is non-empty it filters by a
// case-insensitive title match and ranks by keyword hit count before recency;
// otherwise it seeds the most recent rows. maxAgeDays > 0 restricts the pass to
// rows seen within that window (last_seen_at is UTC 'YYYY-MM-DD HH:MM:SS' text
// in both dialects, so a Go-formatted cutoff compares correctly). It reports
// rows newly inserted into the tenant queue and rows the SELECT scanned (so the
// caller can tell "no inventory matched" apart from "all already seeded").
func (r *Repository) seedFromGlobal(limit int, keywords []string, maxAgeDays int) (inserted, scanned int, err error) {
	q := `
		SELECT id, title, company, url, platform, location, posted_at, scraped_at, description
		FROM global_jobs
		WHERE COALESCE(description, '') <> ''`
	args := make([]any, 0, 2*len(keywords)+2)
	if maxAgeDays > 0 {
		cutoff := time.Now().UTC().AddDate(0, 0, -maxAgeDays).Format("2006-01-02 15:04:05")
		q += " AND last_seen_at >= ?"
		args = append(args, cutoff)
	}
	if len(keywords) > 0 {
		clauses := make([]string, len(keywords))
		for i, kw := range keywords {
			clauses[i] = "LOWER(title) LIKE ?"
			args = append(args, "%"+kw+"%")
		}
		q += " AND (" + strings.Join(clauses, " OR ") + ")"
	}
	if len(keywords) > 0 {
		hits := make([]string, len(keywords))
		for i, kw := range keywords {
			hits[i] = "(CASE WHEN LOWER(title) LIKE ? THEN 1 ELSE 0 END)"
			args = append(args, "%"+kw+"%")
		}
		q += " ORDER BY (" + strings.Join(hits, " + ") + ") DESC, last_seen_at DESC LIMIT ?"
	} else {
		q += " ORDER BY last_seen_at DESC LIMIT ?"
	}
	args = append(args, limit)

	rows, err := r.query(q, args...)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()

	for rows.Next() {
		scanned++
		var lead model.Lead
		var title, company, url, platform, location, postedAt, scrapedAt, description sql.NullString
		if err := rows.Scan(
			&lead.ID,
			&title,
			&company,
			&url,
			&platform,
			&location,
			&postedAt,
			&scrapedAt,
			&description,
		); err != nil {
			return inserted, scanned, err
		}
		lead.Title = title.String
		lead.Company = company.String
		lead.DirectApplyURL = url.String
		lead.ATSPlatformName = platform.String
		lead.Location = location.String
		lead.PostedAt = postedAt.String
		lead.ScrapedTimestamp = scrapedAt.String
		lead.Description = description.String
		ok, err := r.insertScrapedLead(lead, false)
		if err != nil {
			return inserted, scanned, err
		}
		if ok {
			inserted++
		}
	}
	return inserted, scanned, rows.Err()
}

// CountGlobalJobsCache returns the size of the cross-tenant global_jobs cache:
// total cached listings and the distinct company count (case-insensitive). The
// table is global, so no user_id filter applies. Used to publish the
// jsa_global_jobs_total / jsa_global_jobs_companies gauges.
func (r *Repository) CountGlobalJobsCache() (rows, companies int, err error) {
	err = r.queryRow(`
		SELECT COUNT(*), COUNT(DISTINCT LOWER(company))
		FROM global_jobs`).Scan(&rows, &companies)
	return rows, companies, err
}
