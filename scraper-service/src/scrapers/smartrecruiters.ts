/**
 * SmartRecruiters job-board scraper (api.smartrecruiters.com). Two-phase, same
 * shape as gem.ts: GET the public Posting API to list a company's board
 * (paginated, 100 postings per page) then GET each posting for its description
 * (the list omits the body). The per-tenant path (scrapeSmartRecruiters) keeps
 * title matches; board mode (scrapeSmartRecruitersBoards) returns every posting
 * with no filtering. A board is identified by its SmartRecruiters company
 * identifier, the {slug} in api.smartrecruiters.com/v1/companies/{slug}, which
 * is also the jobs.smartrecruiters.com/{slug} vanity path.
 */
import { sleep, safeFetch } from '../lib/http.js';
import { stripHtml } from '../lib/html.js';
import { matchesSearchTerms } from '../lib/search.js';
import { companyConfig } from '../lib/config.js';
import { makeJobLead } from '../lib/joblead.js';
import type { BoardScrapeOutcome } from '../lib/boardfetch.js';
import type { JobLead, BoardFetchResult } from '../types.js';

// The public, unauthenticated SmartRecruiters Posting API base and the vanity
// apply host (jobs.smartrecruiters.com/{slug}/{id} resolves to a posting).
const SR_API = 'https://api.smartrecruiters.com/v1/companies';
const SR_APPLY = 'https://jobs.smartrecruiters.com';

// All company boards share one API host, so keep board concurrency modest to
// stay polite (mirrors gem.ts / workday.ts).
const SR_CONCURRENCY = 8;

// Detail fetches for a full board are one request per posting, run in bounded
// chunks so a large board cannot open thousands of sockets at once.
const DETAIL_CONCURRENCY = 10;

// The postings list is paginated; 100 is the API's maximum page size.
const PAGE_SIZE = 100;

// One board cannot pull more than this many postings, so a runaway board (e.g.
// Bosch's ~4.7k open postings) cannot stall the whole global pass. An over-cap
// board is a truncated view, so board mode never feeds it to the closed-by-diff
// pass (mirrors gem.ts / workday.ts BOARD_MAX_JOBS).
const BOARD_MAX_JOBS = 1000;

// Detail sections in the order they read best for the scorer: the role first,
// company boilerplate last. companyDescription is included so the full posting
// reaches the scorer, matching gem.ts folding every field into the description.
const SECTION_ORDER = [
  'jobDescription',
  'qualifications',
  'additionalInformation',
  'companyDescription',
];

interface SRLocation {
  city?: string;
  region?: string;
  country?: string;
  remote?: boolean;
  fullLocation?: string;
}

interface SRListPosting {
  id?: string;
  name?: string;
  location?: SRLocation;
  company?: { name?: string };
}

interface SRListResponse {
  totalFound?: number;
  content?: SRListPosting[];
}

interface SRSection {
  text?: string;
}

interface SRCompensation {
  min?: number;
  max?: number;
  currency?: string;
  period?: string;
}

interface SRDetailResponse {
  jobAd?: { sections?: Record<string, SRSection> };
  compensation?: SRCompensation;
}

/** Human-readable location from a posting; falls back to city/country parts. */
function locationOf(posting: SRListPosting): string {
  const loc = posting.location || {};
  // fullLocation can carry empty segments ("Bengaluru, , India"); split, drop
  // the blanks, and rejoin so the double comma never reaches the record.
  let out = (loc.fullLocation || '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
    .join(', ');
  if (!out) {
    out = [loc.city, loc.country]
      .map((s) => (s || '').trim())
      .filter(Boolean)
      .join(', ');
  }
  if (loc.remote && !/remote/i.test(out)) {
    out = out ? `${out} (Remote)` : 'Remote';
  }
  return out;
}

/** One-line compensation string, empty when the board posts no range. */
function compensationOf(comp?: SRCompensation): string {
  if (!comp) return '';
  const min = comp.min || 0;
  const max = comp.max || 0;
  if (!min && !max) return '';
  const range = min && max ? `${min} - ${max}` : String(min || max);
  return `Compensation: ${range} ${comp.currency || ''} ${comp.period || ''}`
    .replace(/\s+/g, ' ')
    .trim();
}

/** Fold every jobAd section plus the compensation range into one description. */
function descriptionOf(detail: SRDetailResponse): string {
  const sections = detail.jobAd?.sections || {};
  const parts = SECTION_ORDER.map((key) =>
    stripHtml(sections[key]?.text || '')
  );
  parts.push(compensationOf(detail.compensation));
  return parts.filter(Boolean).join(' ');
}

interface BoardListing {
  postings: SRListPosting[];
  label: string;
  total: number;
  // False when any page request failed or returned unparseable JSON: the
  // listing is a partial view of the board, so it must never feed the
  // closed-by-diff pass.
  ok: boolean;
}

/** Page through a board's postings, capped at BOARD_MAX_JOBS. */
async function listBoard(slug: string): Promise<BoardListing> {
  const postings: SRListPosting[] = [];
  let label = slug;
  let total = 0;
  let offset = 0;
  for (;;) {
    const url = `${SR_API}/${encodeURIComponent(
      slug
    )}/postings?limit=${PAGE_SIZE}&offset=${offset}`;
    const res = await safeFetch(url, {}, `smartrecruiters/${slug}`);
    if (!res) return { postings, label, total, ok: false };
    const data = (await res.json().catch(() => null)) as SRListResponse | null;
    if (!data) return { postings, label, total, ok: false };
    total =
      typeof data.totalFound === 'number' ? data.totalFound : postings.length;
    const content = Array.isArray(data.content) ? data.content : [];
    for (const posting of content) {
      if (label === slug && posting.company?.name?.trim()) {
        label = posting.company.name.trim();
      }
      postings.push(posting);
    }
    offset += PAGE_SIZE;
    if (
      content.length < PAGE_SIZE ||
      offset >= total ||
      postings.length >= BOARD_MAX_JOBS
    ) {
      break;
    }
    await sleep(150);
  }
  return { postings, label, total, ok: true };
}

/** Fetch each posting's detail in bounded chunks and map to JobLeads. */
async function fetchDetails(
  slug: string,
  label: string,
  postings: SRListPosting[]
): Promise<JobLead[]> {
  const jobs: JobLead[] = [];
  for (let i = 0; i < postings.length; i += DETAIL_CONCURRENCY) {
    const chunk = postings.slice(i, i + DETAIL_CONCURRENCY);
    const settled = await Promise.allSettled<JobLead>(
      chunk.map(async (posting) => {
        const url = `${SR_API}/${encodeURIComponent(slug)}/postings/${posting.id}`;
        const res = await safeFetch(url, {}, `smartrecruiters/${slug}/detail`);
        const detail = res
          ? ((await res.json().catch(() => null)) as SRDetailResponse | null)
          : null;
        return makeJobLead({
          title: posting.name,
          company: label,
          directApplyUrl: `${SR_APPLY}/${slug}/${posting.id}`,
          atsPlatformName: 'SmartRecruiters',
          scrapedTimestamp: new Date().toISOString(),
          description: detail ? descriptionOf(detail) : '',
          location: locationOf(posting),
        });
      })
    );
    for (const r of settled) if (r.status === 'fulfilled') jobs.push(r.value);
  }
  return jobs;
}

/** Per-tenant path: list each board, keep title matches, fetch their details. */
export async function scrapeSmartRecruiters(): Promise<JobLead[]> {
  const companies = companyConfig().SMARTRECRUITERS_COMPANIES as string[];
  const allJobs: JobLead[] = [];

  for (let i = 0; i < companies.length; i += SR_CONCURRENCY) {
    const batch = companies.slice(i, i + SR_CONCURRENCY);
    const results = await Promise.allSettled(
      batch.map(async (slug) => {
        const { postings, label } = await listBoard(slug);
        const matched = postings.filter(
          (p) => p.id && matchesSearchTerms(p.name || '')
        );
        return fetchDetails(slug, label, matched);
      })
    );
    for (const r of results) {
      if (r.status === 'fulfilled') allJobs.push(...r.value);
    }
    if (i + SR_CONCURRENCY < companies.length) await sleep(300);
  }

  return allJobs;
}

interface BoardResult {
  label: string;
  leads: JobLead[];
  complete: boolean;
}

async function scrapeBoard(slug: string): Promise<BoardResult> {
  const { postings, label, total, ok } = await listBoard(slug);
  const withId = postings.filter((p) => p.id);
  // An over-cap board was truncated at BOARD_MAX_JOBS, so it must not feed the
  // closed-by-diff pass.
  const complete = ok && total <= BOARD_MAX_JOBS;
  const leads = ok
    ? await fetchDetails(slug, label, withId.slice(0, BOARD_MAX_JOBS))
    : [];
  return { label, leads, complete };
}

/**
 * Board mode: scrape an explicit list of SmartRecruiters boards in full (no
 * search-term filtering). Reads no profile config. Each board reports a
 * BoardFetchResult: `failed` when its listing came back broken or truncated, so
 * the closed-by-diff pass skips it. The `company` on each result is the label
 * its leads are stored under, the key that pass matches on.
 */
export async function scrapeSmartRecruitersBoards(
  slugs: string[]
): Promise<BoardScrapeOutcome> {
  const leads: JobLead[] = [];
  const results: BoardFetchResult[] = [];

  for (let i = 0; i < slugs.length; i += SR_CONCURRENCY) {
    const batch = slugs.slice(i, i + SR_CONCURRENCY);
    const settled = await Promise.allSettled(batch.map(scrapeBoard));
    batch.forEach((slug, idx) => {
      const r = settled[idx];
      const ok = r?.status === 'fulfilled' && r.value.complete;
      if (r?.status === 'fulfilled') leads.push(...r.value.leads);
      results.push({
        platform: 'smartrecruiters',
        slug,
        company: r?.status === 'fulfilled' ? r.value.label : slug,
        status: ok ? 'ok' : 'failed',
      });
    });
    if (i + SR_CONCURRENCY < slugs.length) await sleep(300);
  }

  return { leads, results };
}
