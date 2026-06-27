/**
 * Generic loop for company-based scrapers, ported from lib/base-scraper.js.
 * Same knobs: 15-company batches, 2 fetch retries with linear backoff, a politeness
 * delay between batches, and the same diagnosable zero-jobs logging. Returns the
 * mapped JobLead[] (validation + id derivation happen later in the worker).
 */
import { sleep, safeFetch } from './http.js';
import { matchesSearchTerms } from './search.js';
import { SCRAPER_DELAY_MS } from './config.js';
import { info, warn } from './log.js';
import type { JobLead } from '../types.js';

const COMPANY_CONCURRENCY = 15;
const FETCH_RETRIES = 2; // retry transient/blocked company fetches before giving up
const RETRY_BACKOFF_MS = 750;

export interface ScrapeCompaniesOptions<Item> {
  companies: string[];
  platform: string;
  delay?: number;
  buildUrl: (company: string) => string;
  parseResponse: (data: unknown, company: string) => Item[];
  matchField: (item: Item) => string;
  mapJob: (item: Item, company: string) => JobLead;
}

export async function scrapeCompanies<Item>(
  opts: ScrapeCompaniesOptions<Item>
): Promise<JobLead[]> {
  const {
    companies,
    platform,
    delay = SCRAPER_DELAY_MS,
    buildUrl,
    parseResponse,
    matchField,
    mapJob,
  } = opts;
  const jobs: JobLead[] = [];
  let fetchFailures = 0; // companies whose fetch never returned usable data
  let raw = 0; // items returned by the API, before the search-term filter

  async function fetchCompany(company: string): Promise<JobLead[]> {
    const url = buildUrl(company);
    let res: Response | null = null;
    for (let attempt = 0; attempt <= FETCH_RETRIES; attempt++) {
      res = await safeFetch(url, {}, `${platform}/${company}`);
      if (res) break;
      if (attempt < FETCH_RETRIES)
        await sleep(RETRY_BACKOFF_MS * (attempt + 1));
    }
    if (!res) {
      fetchFailures++;
      return [];
    }
    let data: unknown;
    try {
      data = await res.json();
    } catch {
      fetchFailures++;
      return [];
    }
    const items = parseResponse(data, company) || [];
    raw += items.length;
    return items
      .filter((item) => matchesSearchTerms(matchField(item)))
      .map((item) => mapJob(item, company));
  }

  for (let i = 0; i < companies.length; i += COMPANY_CONCURRENCY) {
    const batch = companies.slice(i, i + COMPANY_CONCURRENCY);
    const results = await Promise.allSettled(batch.map(fetchCompany));
    for (const r of results) {
      if (r.status === 'fulfilled') jobs.push(...r.value);
    }
    if (i + COMPANY_CONCURRENCY < companies.length) await sleep(delay);
  }

  // Make a silent zero-jobs scrape diagnosable: distinguish "fetches blocked",
  // "boards empty", and "nothing matched the search terms".
  const stats = {
    platform,
    companies: companies.length,
    fetchFailures,
    raw,
    matched: jobs.length,
  };
  if (companies.length === 0) {
    info('No companies configured', stats);
  } else if (fetchFailures >= Math.ceil(companies.length / 2)) {
    warn('Scrape mostly failed — company fetches blocked or timing out', stats);
  } else if (jobs.length === 0) {
    warn('Scrape produced no matching jobs', stats);
  } else {
    info('Scrape stats', stats);
  }

  return jobs;
}
