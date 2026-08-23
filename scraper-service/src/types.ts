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

/**
 * One explicit ATS board for board-mode scraping. Platform uses the worker's
 * lowercase platform keys (greenhouse|lever|ashby|workable|workday). Workday
 * boards additionally need the cluster number (`wd`) and board path (`board`);
 * `label` is the display company name (Workday postings carry no company in
 * the API response, so a missing label falls back to the slug).
 */
export interface BoardRef {
  platform: string;
  slug: string;
  label?: string;
  wd?: number;
  board?: string;
  // Incremental fetch state from the previous cycle (single-request board
  // platforms only). etag is sent as If-None-Match; contentHash is compared
  // against the response body's sha256 when the server offers no ETag. Either
  // signal being current skips parsing an unchanged board.
  etag?: string;
  contentHash?: string;
}

/**
 * Per-board outcome of one board-mode scrape. For the single-request board
 * platforms (greenhouse|lever|ashby) the caller persists etag/contentHash and
 * feeds them back on the next cycle. Workday boards paginate, so they carry no
 * etag state but do report `failed` when the listing came back incomplete
 * (blocked page, BOARD_MAX_JOBS overflow) along with the company name their
 * leads are stored under, so the closed-by-diff pass can skip that board.
 * Workable is single-request per account and reports no outcome.
 */
export interface BoardFetchResult {
  platform: string;
  slug: string;
  status: 'ok' | 'unchanged' | 'failed';
  etag?: string;
  contentHash?: string;
  // Workday only: the company name stamped on this board's leads (label,
  // falling back to the tenant slug), the key the closed diff matches on.
  company?: string;
}

/** Request body for POST /scrape. `platforms` omitted/empty => run all. */
export interface ScrapeRequestBody {
  platforms?: string[];
  // Active tenant's profile dir (hosted Postgres). Omitted on self-host / the
  // worker-only probe, where the worker falls back to its DATA_DIR.
  profileDir?: string;
  // Board mode: scrape exactly these boards, returning ALL live postings with
  // no tenant search-term filtering (term matching happens Go-side when the
  // global harvest is distributed to tenants). When set, `platforms` and
  // `profileDir` are ignored and no profile files are read at all.
  boards?: BoardRef[];
}

/** Response body for POST /scrape. */
export interface ScrapeResponse {
  count: number;
  platforms: string[];
  jobs: IdentifiedLead[];
  // Board mode only: per-board fetch outcomes (see BoardFetchResult).
  boards?: BoardFetchResult[];
}
