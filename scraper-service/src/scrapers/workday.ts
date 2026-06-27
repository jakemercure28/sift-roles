/**
 * Native TS port of scrapers/workday.js. Two-phase per tenant: POST the cxs jobs
 * endpoint once per search term (results deduped by tenant + external id), then
 * GET each posting's detail for the full description and location. Companies are
 * processed in parallel batches. Verified byte-identical by
 * test/workday.parity.test.ts.
 */
import { sleep, safeFetch } from '../lib/http.js';
import { stripHtml } from '../lib/html.js';
import { matchesSearchTerms } from '../lib/search.js';
import { companyConfig } from '../lib/config.js';
import { makeJobLead } from '../lib/joblead.js';
import type { JobLead } from '../types.js';

// How many companies to query in parallel. Workday is tolerant of concurrent
// requests but we cap it to avoid hammering shared infra.
const WORKDAY_CONCURRENCY = 8;

interface WorkdayCompany {
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

interface DetailResult {
  job: WorkdayListJob;
  description: string;
  location: string;
}

async function scrapeCompany({
  sub,
  wd,
  board,
  label,
}: WorkdayCompany): Promise<JobLead[]> {
  const jobs: JobLead[] = [];
  const seen = new Set<string>();
  const baseUrl = `https://${sub}.wd${wd}.myworkdayjobs.com`;
  const listUrl = `${baseUrl}/wday/cxs/${sub}/${board}/jobs`;

  // Run all search terms in parallel for this company
  const termResults = await Promise.allSettled(
    companyConfig().SEARCH_TERMS.map((term) =>
      safeFetch(
        listUrl,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ limit: 20, offset: 0, searchText: term }),
        },
        `workday/${sub}/${term}`
      )
        .then((res) => (res ? res.json() : null))
        .catch(() => null)
    )
  );

  // Collect unique matching jobs across all terms
  const detailFetches: { job: WorkdayListJob; listLocation: string }[] = [];
  for (const r of termResults) {
    if (r.status !== 'fulfilled' || !r.value) continue;
    const value = r.value as { jobPostings?: WorkdayListJob[] };
    for (const job of value.jobPostings || []) {
      const jobId = `workday-${sub}-${job.externalPath?.split('/').pop() || job.title}`;
      if (seen.has(jobId)) continue;
      if (!matchesSearchTerms(job.title || '')) continue;
      seen.add(jobId);
      const listLocation =
        job.locationsText ||
        (Array.isArray(job.bulletFields) ? job.bulletFields[0] : '') ||
        '';
      detailFetches.push({ job, listLocation });
    }
  }

  // Fetch descriptions in parallel
  const detailResults = await Promise.allSettled<DetailResult>(
    detailFetches.map(({ job, listLocation }) => {
      if (!job.externalPath)
        return Promise.resolve({
          job,
          description: '',
          location: listLocation,
        });
      const detailUrl = `${baseUrl}/wday/cxs/${sub}/${board}${job.externalPath}`;
      return safeFetch(detailUrl, {}, `workday/${sub}/detail`)
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
        .catch(() => ({ job, description: '', location: listLocation || '' }));
    })
  );

  for (const r of detailResults) {
    if (r.status !== 'fulfilled') continue;
    const { job, description, location } = r.value;
    jobs.push(
      makeJobLead({
        title: job.title,
        company: label,
        directApplyUrl: `${baseUrl}/en-US/${board}${job.externalPath}`,
        atsPlatformName: 'Workday',
        scrapedTimestamp: new Date().toISOString(),
        description,
        location,
      })
    );
  }

  return jobs;
}

export async function scrapeWorkday(): Promise<JobLead[]> {
  const allJobs: JobLead[] = [];
  const companies = companyConfig().WORKDAY_COMPANIES as WorkdayCompany[];

  // Process companies in parallel batches
  for (let i = 0; i < companies.length; i += WORKDAY_CONCURRENCY) {
    const batch = companies.slice(i, i + WORKDAY_CONCURRENCY);
    const results = await Promise.allSettled(batch.map(scrapeCompany));
    for (const r of results) {
      if (r.status === 'fulfilled') allJobs.push(...r.value);
    }
    if (i + WORKDAY_CONCURRENCY < companies.length) await sleep(300);
  }

  return allJobs;
}
