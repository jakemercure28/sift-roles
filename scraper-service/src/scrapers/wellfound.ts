/**
 * Native TS port of scrapers/wellfound.js. Backed by Remotive's free-text
 * ?search= endpoint, queried once per resume-derived search term and deduped by
 * job id across terms. Verified byte-identical by test/wellfound.parity.test.ts.
 */
import { stripHtml } from '../lib/html.js';
import { safeFetch } from '../lib/http.js';
import { matchesSearchTerms } from '../lib/search.js';
import { companyConfig } from '../lib/config.js';
import { makeJobLead } from '../lib/joblead.js';
import type { JobLead } from '../types.js';

interface RemotiveJob {
  id?: number | string;
  title?: string;
  url?: string;
  company_name?: string;
  description?: string;
  candidate_required_location?: string;
}

export async function scrapeWellfound(): Promise<JobLead[]> {
  const jobs: JobLead[] = [];
  const seen = new Set<unknown>();

  for (const term of companyConfig().SEARCH_TERMS) {
    const url = `https://remotive.com/api/remote-jobs?search=${encodeURIComponent(term)}&limit=100`;
    const res = await safeFetch(url, {}, `remotive/${term}`);
    if (!res) continue;

    let data: unknown;
    try {
      data = await res.json();
    } catch {
      continue;
    }

    for (const job of (data as { jobs?: RemotiveJob[] }).jobs || []) {
      const title = job.title || '';
      if (!matchesSearchTerms(title)) continue;
      if (seen.has(job.id)) continue;
      seen.add(job.id);

      jobs.push(
        makeJobLead({
          title,
          company: job.company_name || '',
          directApplyUrl: job.url || '',
          atsPlatformName: 'Remotive',
          scrapedTimestamp: new Date().toISOString(),
          description: stripHtml(job.description || ''),
          location: job.candidate_required_location || 'Remote',
        })
      );
    }
  }

  return jobs;
}
