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

export async function scrapeWorkable(): Promise<JobLead[]> {
  const jobs: JobLead[] = [];

  for (const company of companyConfig().WORKABLE_COMPANIES) {
    const result = await fetchWorkableAccountJobs(company);

    for (const job of result.jobs || []) {
      if (!matchesSearchTerms(job.title)) continue;
      jobs.push(job);
    }

    await sleep(400);
  }

  return jobs;
}
