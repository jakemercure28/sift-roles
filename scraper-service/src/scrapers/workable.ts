/**
 * Native TS port of scrapers/workable.js. Iterates the configured Workable
 * accounts, fetches each via the multi-endpoint fallback (src/lib/workable.ts),
 * and keeps postings whose title matches the search terms. Verified
 * byte-identical by test/workable.parity.test.ts.
 */
import { sleep } from '../lib/http.js';
import { matchesSearchTerms } from '../lib/search.js';
import { companyConfig } from '../lib/config.js';
import { fetchWorkableAccountJobs } from '../lib/workable.js';
import type { JobLead } from '../types.js';

async function scrapeAccounts(
  companies: string[],
  matchAllTerms: boolean
): Promise<JobLead[]> {
  const jobs: JobLead[] = [];

  for (const company of companies) {
    const result = await fetchWorkableAccountJobs(company);

    for (const job of result.jobs || []) {
      if (!matchAllTerms && !matchesSearchTerms(job.title)) continue;
      jobs.push(job);
    }

    await sleep(400);
  }

  return jobs;
}

export async function scrapeWorkable(): Promise<JobLead[]> {
  return scrapeAccounts(companyConfig().WORKABLE_COMPANIES, false);
}

/**
 * Board mode: scrape an explicit list of Workable accounts, keeping every
 * posting (term matching happens Go-side when the global harvest is
 * distributed to tenants). Reads no profile config.
 */
export async function scrapeWorkableBoards(
  companies: string[]
): Promise<JobLead[]> {
  return scrapeAccounts(companies, true);
}
