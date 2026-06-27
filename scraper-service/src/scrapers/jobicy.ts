/**
 * Native TS port of scrapers/jobicy.js. Single Remotive endpoint fixed to the
 * devops-sysadmin category; canonical ATS links are detected for apply-url
 * canonicalization. Verified byte-identical by test/jobicy.parity.test.ts.
 */
import { stripHtml } from '../lib/html.js';
import { safeFetch } from '../lib/http.js';
import { detectAts } from '../lib/ats.js';
import { matchesSearchTerms } from '../lib/search.js';
import { makeJobLead } from '../lib/joblead.js';
import type { JobLead } from '../types.js';

interface RemotiveJob {
  title?: string;
  url?: string;
  company_name?: string;
  description?: string;
  candidate_required_location?: string;
  publication_date?: string;
}

export async function scrapeJobicy(): Promise<JobLead[]> {
  const jobs: JobLead[] = [];
  const url =
    'https://remotive.com/api/remote-jobs?category=devops-sysadmin&limit=100';
  const res = await safeFetch(url, {}, 'remotive');
  if (!res) return jobs;

  let data: unknown;
  try {
    data = await res.json();
  } catch {
    return jobs;
  }

  for (const job of (data as { jobs?: RemotiveJob[] }).jobs || []) {
    const title = job.title || '';
    if (!matchesSearchTerms(title)) continue;

    const jobUrl = job.url || '';
    const ats = detectAts(jobUrl);

    jobs.push(
      makeJobLead({
        title,
        company: job.company_name || '',
        directApplyUrl: jobUrl,
        atsPlatformName: ats ? ats.platform : 'Remotive',
        scrapedTimestamp: new Date().toISOString(),
        description: stripHtml(job.description || ''),
        location: job.candidate_required_location || 'Remote',
        postedAt: job.publication_date || '',
      })
    );
  }

  return jobs;
}
