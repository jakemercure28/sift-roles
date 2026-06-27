/**
 * Native TS port of scrapers/remoteok.js. RemoteOK's single API endpoint returns
 * an array whose first element is a legal notice (skipped). Where a posting
 * carries a canonical ATS apply_url, that URL/platform is preferred over the
 * RemoteOK aggregator link. Verified byte-identical by test/remoteok.parity.test.ts.
 */
import { stripHtml } from '../lib/html.js';
import { safeFetch } from '../lib/http.js';
import { detectAts } from '../lib/ats.js';
import { matchesSearchTerms } from '../lib/search.js';
import { makeJobLead } from '../lib/joblead.js';
import type { JobLead } from '../types.js';

interface RemoteOkJob {
  position?: string;
  apply_url?: string;
  url?: string;
  slug?: string;
  company?: string;
  description?: string;
  location?: string;
  epoch?: number;
}

export async function scrapeRemoteOK(): Promise<JobLead[]> {
  const jobs: JobLead[] = [];
  const url = 'https://remoteok.com/api';
  const res = await safeFetch(
    url,
    { headers: { 'User-Agent': 'job-search-bot/1.0 (personal use)' } },
    'remoteok'
  );
  if (!res) return jobs;

  let data: unknown;
  try {
    data = await res.json();
  } catch {
    return jobs;
  }

  // First element is a legal notice object, skip it
  const list = Array.isArray(data) ? (data as RemoteOkJob[]).slice(1) : [];
  for (const job of list) {
    const title = job.position || '';
    if (!matchesSearchTerms(title)) continue;

    // Prefer the canonical ATS URL (apply_url) over the RemoteOK aggregator link
    const ats = detectAts(job.apply_url || '');
    const canonicalUrl = ats
      ? job.apply_url
      : job.url || `https://remoteok.com/remote-jobs/${job.slug}`;

    jobs.push(
      makeJobLead({
        title,
        company: job.company || '',
        directApplyUrl: canonicalUrl,
        atsPlatformName: ats ? ats.platform : 'RemoteOK',
        scrapedTimestamp: new Date().toISOString(),
        description: stripHtml(job.description || ''),
        location: job.location || 'Remote',
        postedAt: job.epoch
          ? new Date(job.epoch * 1000).toISOString().slice(0, 10)
          : '',
      })
    );
  }

  return jobs;
}
