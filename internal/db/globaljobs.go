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

func descriptionHash(description string) string {
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
		descriptionHash(lead.Description),
		r.userID,
	)
	return err
}

// SeedTenantJobsFromGlobal copies recent global job descriptions into this
// tenant's jobs queue as pending/unscored rows. It is idempotent per tenant via
// (user_id, global_job_id), and bounded so a first run does not flood the tenant.
func (r *Repository) SeedTenantJobsFromGlobal(limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	rows, err := r.query(`
		SELECT id, title, company, url, platform, location, posted_at, scraped_at, description
		FROM global_jobs
		WHERE COALESCE(description, '') <> ''
		ORDER BY last_seen_at DESC
		LIMIT ?`, limit)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	inserted := 0
	for rows.Next() {
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
			return inserted, err
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
			return inserted, err
		}
		if ok {
			inserted++
		}
	}
	return inserted, rows.Err()
}
