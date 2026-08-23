/**
 * Gem job-board scraper (jobs.gem.com). Two-phase, same shape as workday.ts:
 * POST the public GraphQL endpoint once to list a board's postings, then POST
 * once per posting for its description + compensation (the list query omits
 * both). The per-tenant path (scrapeGem) keeps title matches; board mode
 * (scrapeGemBoards) returns every posting with no filtering. Gem identifies a
 * board by its vanity slug, the path segment in jobs.gem.com/{slug}, which is
 * also the GraphQL boardId.
 */
import { sleep, safeFetch } from '../lib/http.js';
import { stripHtml } from '../lib/html.js';
import { matchesSearchTerms } from '../lib/search.js';
import { companyConfig } from '../lib/config.js';
import { makeJobLead } from '../lib/joblead.js';
import type { BoardScrapeOutcome } from '../lib/boardfetch.js';
import type { JobLead, BoardFetchResult } from '../types.js';

// The public, unauthenticated GraphQL endpoint the jobs.gem.com board app calls.
const GEM_GRAPHQL = 'https://jobs.gem.com/api/public/graphql';

// How many boards to query in parallel. Every board hits the same shared host,
// so we keep this modest to stay polite (mirrors Workday's cap).
const GEM_CONCURRENCY = 8;

// Detail fetches for a full board are one request per posting, so they run in
// bounded chunks; and one board cannot pull more than this many postings, so a
// runaway board cannot stall the whole global pass.
const DETAIL_CONCURRENCY = 10;
const BOARD_MAX_JOBS = 1000;

// Only the fields the pipeline needs; the live board app requests far more.
const LIST_QUERY = `
  query JobBoardList($boardId: String!) {
    oatsExternalJobPostings(boardId: $boardId) {
      jobPostings {
        extId
        title
        locations { name }
      }
    }
    jobBoardExternal(vanityUrlPath: $boardId) { teamDisplayName }
  }
`;

const DETAIL_QUERY = `
  query ExternalJobPostingQuery($boardId: String!, $extId: String!) {
    oatsExternalJobPosting(boardId: $boardId, extId: $extId) {
      descriptionHtml
      compensationHtml
    }
  }
`;

interface GemLocation {
  name?: string;
}

interface GemListPosting {
  extId?: string;
  title?: string;
  locations?: GemLocation[];
}

interface GemListResponse {
  data?: {
    oatsExternalJobPostings?: { jobPostings?: GemListPosting[] };
    jobBoardExternal?: { teamDisplayName?: string };
  } | null;
}

interface GemDetailResponse {
  data?: {
    oatsExternalJobPosting?: {
      descriptionHtml?: string;
      compensationHtml?: string;
    };
  } | null;
}

/** POST a GraphQL operation to Gem; returns the parsed JSON or null on failure. */
async function gemQuery(
  operationName: string,
  query: string,
  variables: Record<string, string>,
  label: string
): Promise<unknown> {
  const res = await safeFetch(
    GEM_GRAPHQL,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ operationName, variables, query }),
    },
    label
  );
  if (!res) return null;
  return res.json().catch(() => null);
}

function locationOf(posting: GemListPosting): string {
  return (posting.locations || [])
    .map((l) => (l.name || '').trim())
    .filter(Boolean)
    .join(' | ');
}

interface BoardListing {
  postings: GemListPosting[];
  label: string;
  // False when the list request failed or returned a GraphQL error (data:null):
  // the listing is not a trustworthy full view of the board, so it must never
  // feed the closed-by-diff pass.
  ok: boolean;
}

async function listBoard(slug: string): Promise<BoardListing> {
  const res = (await gemQuery(
    'JobBoardList',
    LIST_QUERY,
    { boardId: slug },
    `gem/${slug}`
  )) as GemListResponse | null;
  const data = res?.data;
  if (!data) return { postings: [], label: slug, ok: false };
  return {
    postings: data.oatsExternalJobPostings?.jobPostings || [],
    label: data.jobBoardExternal?.teamDisplayName || slug,
    ok: true,
  };
}

/** Fetch each posting's detail in bounded chunks and map to JobLeads. */
async function fetchDetails(
  slug: string,
  label: string,
  postings: GemListPosting[]
): Promise<JobLead[]> {
  const jobs: JobLead[] = [];
  for (let i = 0; i < postings.length; i += DETAIL_CONCURRENCY) {
    const chunk = postings.slice(i, i + DETAIL_CONCURRENCY);
    const settled = await Promise.allSettled<JobLead>(
      chunk.map(async (posting) => {
        const detail = (await gemQuery(
          'ExternalJobPostingQuery',
          DETAIL_QUERY,
          { boardId: slug, extId: posting.extId as string },
          `gem/${slug}/detail`
        )) as GemDetailResponse | null;
        const info = detail?.data?.oatsExternalJobPosting || {};
        // Fold compensation into the description so the salary range reaches
        // the scorer (the list/detail split otherwise hides it in its own
        // field, which nothing downstream reads).
        const description = [
          stripHtml(info.descriptionHtml || ''),
          stripHtml(info.compensationHtml || ''),
        ]
          .filter(Boolean)
          .join(' ');
        return makeJobLead({
          title: posting.title,
          company: label,
          directApplyUrl: `https://jobs.gem.com/${slug}/${posting.extId}`,
          atsPlatformName: 'Gem',
          scrapedTimestamp: new Date().toISOString(),
          description,
          location: locationOf(posting),
        });
      })
    );
    for (const r of settled) if (r.status === 'fulfilled') jobs.push(r.value);
  }
  return jobs;
}

/** Per-tenant path: list each board, keep title matches, fetch their details. */
export async function scrapeGem(): Promise<JobLead[]> {
  const companies = companyConfig().GEM_COMPANIES as string[];
  const allJobs: JobLead[] = [];

  for (let i = 0; i < companies.length; i += GEM_CONCURRENCY) {
    const batch = companies.slice(i, i + GEM_CONCURRENCY);
    const results = await Promise.allSettled(
      batch.map(async (slug) => {
        const { postings, label } = await listBoard(slug);
        const matched = postings.filter(
          (p) => p.extId && matchesSearchTerms(p.title || '')
        );
        return fetchDetails(slug, label, matched);
      })
    );
    for (const r of results) {
      if (r.status === 'fulfilled') allJobs.push(...r.value);
    }
    if (i + GEM_CONCURRENCY < companies.length) await sleep(300);
  }

  return allJobs;
}

interface BoardResult {
  label: string;
  leads: JobLead[];
  complete: boolean;
}

async function scrapeBoard(slug: string): Promise<BoardResult> {
  const { postings, label, ok } = await listBoard(slug);
  const withId = postings.filter((p) => p.extId);
  // Cap detail fetches; an over-cap board is a truncated view, so it must not
  // feed the closed-by-diff pass (mirrors Workday's BOARD_MAX_JOBS overflow).
  const capped = withId.slice(0, BOARD_MAX_JOBS);
  const complete = ok && withId.length <= BOARD_MAX_JOBS;
  const leads = ok ? await fetchDetails(slug, label, capped) : [];
  return { label, leads, complete };
}

/**
 * Board mode: scrape an explicit list of Gem boards in full (no search-term
 * filtering). Reads no profile config. Each board reports a BoardFetchResult:
 * `failed` when its listing came back broken or truncated, so the closed-by-diff
 * pass skips it. The `company` on each result is the label its leads are stored
 * under, the key that pass matches on.
 */
export async function scrapeGemBoards(
  slugs: string[]
): Promise<BoardScrapeOutcome> {
  const leads: JobLead[] = [];
  const results: BoardFetchResult[] = [];

  for (let i = 0; i < slugs.length; i += GEM_CONCURRENCY) {
    const batch = slugs.slice(i, i + GEM_CONCURRENCY);
    const settled = await Promise.allSettled(batch.map(scrapeBoard));
    batch.forEach((slug, idx) => {
      const r = settled[idx];
      const ok = r?.status === 'fulfilled' && r.value.complete;
      if (r?.status === 'fulfilled') leads.push(...r.value.leads);
      results.push({
        platform: 'gem',
        slug,
        company: r?.status === 'fulfilled' ? r.value.label : slug,
        status: ok ? 'ok' : 'failed',
      });
    });
    if (i + GEM_CONCURRENCY < slugs.length) await sleep(300);
  }

  return { leads, results };
}
