/**
 * The strict JobLead contract — must stay byte-compatible with the Node side
 * (`makeJobLead` in ../../lib/job-lead.js) and the pydantic contract
 * (../../contracts/job_lead.py). All fields are trimmed strings.
 */
export interface JobLead {
  title: string;
  company: string;
  description: string;
  direct_apply_url: string;
  ats_platform_name: string;
  scraped_timestamp: string;
  location: string;
  posted_at: string;
}

/** The 8 contract fields, in order — mirrors CONTRACT_FIELDS in lib/job-lead.js. */
export const CONTRACT_FIELDS = [
  'title',
  'company',
  'description',
  'direct_apply_url',
  'ats_platform_name',
  'scraped_timestamp',
  'location',
  'posted_at',
] as const satisfies ReadonlyArray<keyof JobLead>;

/**
 * A validated lead plus its canonical job id. `id` is derived by the single
 * source of truth — deriveJobId in ../../lib/job-lead.js — so the Go writer can
 * insert without reimplementing identity (which would risk drift). It is added
 * to the response object AFTER contract validation, so the inner 8 fields stay
 * the strict JobLead shape.
 */
export interface IdentifiedLead extends JobLead {
  id: string;
}

/** Request body for POST /scrape. `platforms` omitted/empty => run all. */
export interface ScrapeRequestBody {
  platforms?: string[];
  // Active tenant's profile dir (hosted Postgres). Omitted on self-host / the
  // worker-only probe, where the worker falls back to its DATA_DIR.
  profileDir?: string;
}

/** Response body for POST /scrape. */
export interface ScrapeResponse {
  count: number;
  platforms: string[];
  jobs: IdentifiedLead[];
}
