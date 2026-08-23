/**
 * Native TS port of scrapers/workday.js. Two-phase per tenant: list postings
 * from the cxs jobs endpoint, then GET each posting's detail for the full
 * description and location. The per-tenant path paginates each search term and
 * keeps title matches; board mode paginates the same endpoint with an empty
 * search to return the whole board. Companies are processed in parallel
 * batches. Term-mode output is verified byte-identical by
 * test/workday.parity.test.ts.
 */
import { sleep, safeFetch, HostBreaker } from '../lib/http.js';
import { stripHtml } from '../lib/html.js';
import { matchesSearchTerms } from '../lib/search.js';
import { companyConfig } from '../lib/config.js';
import { makeJobLead } from '../lib/joblead.js';
import type { BoardScrapeOutcome } from '../lib/boardfetch.js';
import type { JobLead, BoardFetchResult } from '../types.js';

// How many companies to query in parallel. Workday is tolerant of concurrent
// requests but we cap it to avoid hammering shared infra.
const WORKDAY_CONCURRENCY = 8;

// Board mode caps: cxs pages are 20 postings; stop after this many postings per
// board so one megaboard cannot stall the whole global pass. Detail fetches for
// a full board can number in the hundreds, so they run in bounded chunks.
const BOARD_PAGE_SIZE = 20;
const BOARD_MAX_JOBS = 1000;
const DETAIL_CONCURRENCY = 10;

// Term-mode cap: a search term is already server-side filtered, so anything
// past a few pages is noise, and the cap bounds request volume if a server
// ignores `offset` and keeps serving full pages.
const TERM_MAX_JOBS = 200;

export interface WorkdayCompany {
  sub: string;
  wd: number;
  board: string;
  label: string;
}

interface WorkdayListJob {
  title?: string;
  externalPath?: string;
  locationsText?: string;
  bulletFields?: string[];
}

interface WorkdayDetail {
  jobPostingInfo?: {
    jobDescription?: string;
    location?: string;
    additionalLocations?: string[];
  };
}

interface ListedJob {
  job: WorkdayListJob;
  listLocation: string;
}

interface DetailResult {
  job: WorkdayListJob;
  description: string;
  location: string;
}

function listLocationOf(job: WorkdayListJob): string {
  return (
    job.locationsText ||
    (Array.isArray(job.bulletFields) ? job.bulletFields[0] : '') ||
    ''
  );
}

function postJobs(
  listUrl: string,
  searchText: string,
  offset: number,
  limit: number,
  label: string,
  breaker?: HostBreaker
): Promise<unknown> {
  return safeFetch(
    listUrl,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ limit, offset, searchText }),
    },
    label,
    breaker
  )
    .then((res) => (res ? res.json() : null))
    .catch(() => null);
}

/**
 * Paginate one search term. Stops on a fetch failure or a short page. Unlike
 * board mode there is no completeness tracking: term mode never feeds the
 * closed-by-diff pass, so a truncated listing only means fewer leads this
 * cycle, never wrongly closed jobs.
 */
async function listTermPages(
  listUrl: string,
  sub: string,
  term: string,
  breaker?: HostBreaker
): Promise<WorkdayListJob[]> {
  const postings: WorkdayListJob[] = [];
  for (let offset = 0; offset < TERM_MAX_JOBS; offset += BOARD_PAGE_SIZE) {
    const value = (await postJobs(
      listUrl,
      term,
      offset,
      BOARD_PAGE_SIZE,
      `workday/${sub}/${term}@${offset}`,
      breaker
    )) as { jobPostings?: WorkdayListJob[] } | null;
    if (!value) break;
    const page = value.jobPostings || [];
    postings.push(...page);
    if (page.length < BOARD_PAGE_SIZE) break;
  }
  return postings;
}

/** Term mode: paginate each search term, keeping title matches (per-tenant path). */
async function listByTerms(
  listUrl: string,
  sub: string,
  breaker?: HostBreaker
): Promise<ListedJob[]> {
  const termResults = await Promise.allSettled(
    companyConfig().SEARCH_TERMS.map((term) =>
      listTermPages(listUrl, sub, term, breaker)
    )
  );

  const seen = new Set<string>();
  const listed: ListedJob[] = [];
  for (const r of termResults) {
    if (r.status !== 'fulfilled') continue;
    for (const job of r.value) {
      const jobId = `workday-${sub}-${job.externalPath?.split('/').pop() || job.title}`;
      if (seen.has(jobId)) continue;
      if (!matchesSearchTerms(job.title || '')) continue;
      seen.add(jobId);
      listed.push({ job, listLocation: listLocationOf(job) });
    }
  }
  return listed;
}

interface BoardListing {
  listed: ListedJob[];
  // False when any page fetch failed (WAF block, timeout) or the board holds
  // more postings than BOARD_MAX_JOBS: the listing is a truncated view of the
  // board, so absence from it proves nothing and it must never feed the
  // closed-by-diff pass.
  complete: boolean;
}

/** Board mode: paginate the whole board with an empty search, no title filter. */
async function listFullBoard(
  listUrl: string,
  sub: string,
  breaker?: HostBreaker
): Promise<BoardListing> {
  const seen = new Set<string>();
  const listed: ListedJob[] = [];
  for (let offset = 0; offset < BOARD_MAX_JOBS; offset += BOARD_PAGE_SIZE) {
    const value = (await postJobs(
      listUrl,
      '',
      offset,
      BOARD_PAGE_SIZE,
      `workday/${sub}/board@${offset}`,
      breaker
    )) as { jobPostings?: WorkdayListJob[]; total?: number } | null;
    if (!value) return { listed, complete: false };
    const postings = value.jobPostings || [];
    for (const job of postings) {
      const jobId = `workday-${sub}-${job.externalPath?.split('/').pop() || job.title}`;
      if (seen.has(jobId)) continue;
      seen.add(jobId);
      listed.push({ job, listLocation: listLocationOf(job) });
    }
    // The server's total is untrustworthy: mid-pagination pages have been
    // observed reporting total: 0 while still serving postings (Accenture,
    // Autodesk 2026-07-04), and reading that as "past the end" closed live
    // jobs. Treat a non-positive total as unknown, and only accept a
    // total-based end when we actually hold that many postings.
    const total =
      typeof value.total === 'number' && value.total > 0 ? value.total : null;
    if (postings.length < BOARD_PAGE_SIZE) {
      // A short or empty page is the end of the board only when a positive
      // total corroborates what we hold. Without that corroboration the page
      // is indistinguishable from WAF garbage ({jobPostings: [], total: 0}
      // served mid-pagination, observed 2026-07-04 on autodesk/visa/intel),
      // which read as a clean end and fed truncated listings to the closed
      // diff as "complete". A board empty from its first page stays complete:
      // zero-lead boards never enter the diff, and this keeps parked boards
      // from logging as failed every cycle.
      if (total === null) {
        return { listed, complete: listed.length === 0 && offset === 0 };
      }
      return { listed, complete: listed.length >= total };
    }
    if (total !== null && offset + BOARD_PAGE_SIZE >= total) {
      return { listed, complete: listed.length >= total };
    }
  }
  return { listed, complete: false };
}

/**
 * Fetch posting details in bounded chunks and map to JobLeads.
 *
 * Returns `throttled` when the host tripped its breaker part-way through, so the
 * caller can mark the board incomplete: the listing may be whole while the
 * details are not, and a board that looks thin because we were being refused
 * must never reach the closed-by-diff pass as authoritative.
 */
async function fetchDetails(
  baseUrl: string,
  { sub, board, label }: WorkdayCompany,
  listed: ListedJob[],
  breaker?: HostBreaker
): Promise<{ jobs: JobLead[]; throttled: boolean }> {
  const jobs: JobLead[] = [];
  for (let i = 0; i < listed.length; i += DETAIL_CONCURRENCY) {
    // Stop early rather than walking the rest of the board to be refused once
    // per posting; that loop is what produced thousands of 429s per pass.
    if (breaker?.blocked(baseUrl)) return { jobs, throttled: true };
    const chunk = listed.slice(i, i + DETAIL_CONCURRENCY);
    const detailResults = await Promise.allSettled<DetailResult>(
      chunk.map(({ job, listLocation }) => {
        if (!job.externalPath)
          return Promise.resolve({
            job,
            description: '',
            location: listLocation,
          });
        const detailUrl = `${baseUrl}/wday/cxs/${sub}/${board}${job.externalPath}`;
        return safeFetch(detailUrl, {}, `workday/${sub}/detail`, breaker)
          .then((res) => (res ? res.json() : null))
          .then((detail) => {
            const info = (detail as WorkdayDetail | null)?.jobPostingInfo || {};
            const extra = Array.isArray(info.additionalLocations)
              ? info.additionalLocations.filter(Boolean)
              : [];
            const location =
              [info.location, ...extra].filter(Boolean).join(' | ') ||
              listLocation ||
              '';
            return {
              job,
              description: stripHtml(info.jobDescription || ''),
              location,
            };
          })
          .catch(() => ({
            job,
            description: '',
            location: listLocation || '',
          }));
      })
    );

    for (const r of detailResults) {
      if (r.status !== 'fulfilled') continue;
      const { job, description, location } = r.value;
      jobs.push(
        makeJobLead({
          title: job.title,
          // Registry-harvested entries carry no label; fall back to the tenant
          // slug or validation drops the lead for a blank company.
          company: label || sub,
          directApplyUrl: `${baseUrl}/en-US/${board}${job.externalPath}`,
          atsPlatformName: 'Workday',
          scrapedTimestamp: new Date().toISOString(),
          description,
          location,
        })
      );
    }
  }
  return { jobs, throttled: breaker?.blocked(baseUrl) ?? false };
}

async function scrapeCompany(
  company: WorkdayCompany,
  breaker: HostBreaker
): Promise<JobLead[]> {
  const { sub, wd, board } = company;
  const baseUrl = `https://${sub}.wd${wd}.myworkdayjobs.com`;
  const listUrl = `${baseUrl}/wday/cxs/${sub}/${board}/jobs`;
  const listed = await listByTerms(listUrl, sub, breaker);
  return (await fetchDetails(baseUrl, company, listed, breaker)).jobs;
}

export async function scrapeWorkday(): Promise<JobLead[]> {
  const companies = companyConfig().WORKDAY_COMPANIES as WorkdayCompany[];
  const allJobs: JobLead[] = [];
  // One breaker for the whole pass: each Workday tenant is its own host, so a
  // tenant that throttles us stays parked across its remaining batches.
  const breaker = new HostBreaker();

  // Process companies in parallel batches
  for (let i = 0; i < companies.length; i += WORKDAY_CONCURRENCY) {
    const batch = companies.slice(i, i + WORKDAY_CONCURRENCY);
    const results = await Promise.allSettled(
      batch.map((c) => scrapeCompany(c, breaker))
    );
    for (const r of results) {
      if (r.status === 'fulfilled') allJobs.push(...r.value);
    }
    if (i + WORKDAY_CONCURRENCY < companies.length) await sleep(300);
  }

  return allJobs;
}

async function scrapeBoardCompany(
  company: WorkdayCompany,
  breaker: HostBreaker
): Promise<{ leads: JobLead[]; complete: boolean }> {
  const { sub, wd, board } = company;
  const baseUrl = `https://${sub}.wd${wd}.myworkdayjobs.com`;
  const listUrl = `${baseUrl}/wday/cxs/${sub}/${board}/jobs`;
  const { listed, complete } = await listFullBoard(listUrl, sub, breaker);
  const { jobs, throttled } = await fetchDetails(
    baseUrl,
    company,
    listed,
    breaker
  );
  // Throttled details mean we hold fewer leads than the board really has, which
  // is exactly the shape that fed the mass-close incidents. Never authoritative.
  return { leads: jobs, complete: complete && !throttled };
}

/**
 * Board mode: scrape an explicit list of Workday boards in full (paginated,
 * no search-term filtering). Reads no profile config. Each board reports a
 * BoardFetchResult: `failed` when its listing was incomplete (blocked page,
 * BOARD_MAX_JOBS overflow), so the closed-by-diff pass skips it — partial
 * fetches were closing live postings that fell past the truncation point.
 */
export async function scrapeWorkdayBoards(
  companies: WorkdayCompany[]
): Promise<BoardScrapeOutcome> {
  const leads: JobLead[] = [];
  const results: BoardFetchResult[] = [];
  // One breaker for the whole pass: each Workday tenant is its own host, so a
  // tenant that throttles us stays parked across its remaining batches.
  const breaker = new HostBreaker();

  for (let i = 0; i < companies.length; i += WORKDAY_CONCURRENCY) {
    const batch = companies.slice(i, i + WORKDAY_CONCURRENCY);
    const settled = await Promise.allSettled(
      batch.map((c) => scrapeBoardCompany(c, breaker))
    );
    batch.forEach((c, idx) => {
      const r = settled[idx];
      const ok = r?.status === 'fulfilled' && r.value.complete;
      if (r?.status === 'fulfilled') leads.push(...r.value.leads);
      results.push({
        platform: 'workday',
        slug: c.sub,
        company: c.label || c.sub,
        status: ok ? 'ok' : 'failed',
      });
    });
    if (i + WORKDAY_CONCURRENCY < companies.length) await sleep(300);
  }

  return { leads, results };
}
