/**
 * Native TS port of scrapers/arbeitnow.js. Single free public endpoint; location
 * may be an array (joined) and falls back to Remote when the role is flagged
 * remote. Verified byte-identical by test/arbeitnow.parity.test.ts.
 */
import { stripHtml } from '../lib/html.js';
import { safeFetch } from '../lib/http.js';
import { detectAts } from '../lib/ats.js';
import { matchesSearchTerms } from '../lib/search.js';
import { makeJobLead } from '../lib/joblead.js';
import type { JobLead } from '../types.js';

interface ArbeitnowJob {
  title?: string;
  url?: string;
  company_name?: string;
  description?: string;
  location?: string | string[];
  remote?: boolean;
  created_at?: string;
}

export async function scrapeArbeitnow(): Promise<JobLead[]> {
  const jobs: JobLead[] = [];
  // Free public API — remote international jobs, good for remote-anywhere roles
  const url = 'https://www.arbeitnow.com/api/job-board-api';
  const res = await safeFetch(url, {}, 'arbeitnow');
  if (!res) return jobs;

  let data: unknown;
  try {
    data = await res.json();
  } catch {
    return jobs;
  }

  for (const job of (data as { data?: ArbeitnowJob[] }).data || []) {
    const title = job.title || '';
    if (!matchesSearchTerms(title)) continue;

    const jobUrl = job.url || '';
    const ats = detectAts(jobUrl);

    const rawLoc = Array.isArray(job.location)
      ? job.location.filter(Boolean).join(', ')
      : job.location || '';
    const location = rawLoc || (job.remote ? 'Remote' : '');
    jobs.push(
      makeJobLead({
        title,
        company: job.company_name || '',
        directApplyUrl: jobUrl,
        atsPlatformName: ats ? ats.platform : 'Arbeitnow',
        scrapedTimestamp: new Date().toISOString(),
        description: stripHtml(job.description || ''),
        location,
        postedAt: job.created_at || '',
      })
    );
  }

  return jobs;
}
