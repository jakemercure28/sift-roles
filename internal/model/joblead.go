// Package model holds the shared domain types that cross the Go service
// boundaries (scraper client, repository, job-id derivation).
package model

// JobLead is the strict scraped-job contract. It must stay byte-compatible with
// the TypeScript worker's JobLead (scraper-service/src/types.ts), the Node
// makeJobLead (lib/job-lead.js), and the pydantic contract (contracts/job_lead.py).
// All fields are plain strings.
type JobLead struct {
	Title            string `json:"title"`
	Company          string `json:"company"`
	Description      string `json:"description"`
	DirectApplyURL   string `json:"direct_apply_url"`
	ATSPlatformName  string `json:"ats_platform_name"`
	ScrapedTimestamp string `json:"scraped_timestamp"`
	Location         string `json:"location"`
	PostedAt         string `json:"posted_at"`
}

// Lead is a validated JobLead plus its canonical job id, as emitted by the
// scraper worker. The id is derived by the single source of truth
// (deriveJobId in lib/job-lead.js), so the Go repository inserts it directly
// rather than reimplementing identity derivation.
type Lead struct {
	JobLead
	ID string `json:"id"`
}
