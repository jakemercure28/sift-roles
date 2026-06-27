/**
 * Native TS port of scrapers/greenhouse.js. Greenhouse exposes a clean public
 * board API; this maps each posting to the strict JobLead. Verified
 * byte-identical to the JS original by test/greenhouse.parity.test.ts.
 */
import { stripHtml } from '../lib/html.js';
import { scrapeCompanies } from '../lib/companies.js';
import { makeJobLead } from '../lib/joblead.js';
import { companyConfig } from '../lib/config.js';
import type { JobLead } from '../types.js';

interface GreenhouseJob {
  title?: string;
  absolute_url?: string;
  content?: string;
  location?: { name?: string };
  offices?: Array<{ name?: string } | null>;
  updated_at?: string;
}

export async function scrapeGreenhouse(): Promise<JobLead[]> {
  return scrapeCompanies<GreenhouseJob>({
    companies: companyConfig().GREENHOUSE_COMPANIES,
    platform: 'greenhouse',
    buildUrl: (company) =>
      `https://boards-api.greenhouse.io/v1/boards/${company}/jobs?content=true`,
    parseResponse: (data) => (data as { jobs?: GreenhouseJob[] }).jobs || [],
    matchField: (job) => job.title || '',
    mapJob: (job, company) => {
      const offices = Array.isArray(job.offices)
        ? job.offices
            .map((o) => o?.name)
            .filter(Boolean)
            .join(' | ')
        : '';
      const location = job.location?.name || offices || '';
      return makeJobLead({
        title: job.title,
        company: company,
        directApplyUrl: job.absolute_url,
        atsPlatformName: 'Greenhouse',
        scrapedTimestamp: new Date().toISOString(),
        description: stripHtml(job.content || ''),
        location,
        postedAt: job.updated_at || '',
      });
    },
  });
}
