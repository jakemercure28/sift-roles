/**
 * Native TS port of scrapers/lever.js. Lever's `?mode=json` endpoint returns a
 * top-level array of postings; the description is reassembled from the intro
 * plus named list sections plus the closing, matching the JS original exactly.
 * Verified byte-identical by test/lever.parity.test.ts.
 */
import { stripHtml } from '../lib/html.js';
import { scrapeCompanies } from '../lib/companies.js';
import { makeJobLead } from '../lib/joblead.js';
import { companyConfig } from '../lib/config.js';
import type { JobLead } from '../types.js';

interface LeverList {
  text?: string;
  content?: string;
}

interface LeverJob {
  text?: string;
  hostedUrl?: string;
  descriptionPlain?: string;
  description?: string;
  lists?: LeverList[];
  closing?: string;
  categories?: { allLocations?: string[]; location?: string };
  createdAt?: number;
}

export async function scrapeLever(): Promise<JobLead[]> {
  return scrapeCompanies<LeverJob>({
    companies: companyConfig().LEVER_COMPANIES,
    platform: 'lever',
    buildUrl: (company) =>
      `https://api.lever.co/v0/postings/${company}?mode=json`,
    parseResponse: (data) => (Array.isArray(data) ? (data as LeverJob[]) : []),
    matchField: (job) => job.text || '',
    mapJob: (job, company) => {
      // Lever splits content into intro + named sections (lists). Concatenate all of them.
      const parts = [job.descriptionPlain || stripHtml(job.description || '')];
      for (const section of job.lists || []) {
        if (section.text) parts.push(section.text + ':');
        if (section.content) parts.push(stripHtml(section.content));
      }
      if (job.closing) parts.push(stripHtml(job.closing));
      const allLocations = Array.isArray(job.categories?.allLocations)
        ? job.categories.allLocations.filter(Boolean)
        : [];
      const location = allLocations.length
        ? allLocations.join(' | ')
        : job.categories?.location || '';
      return makeJobLead({
        title: job.text,
        company: company,
        directApplyUrl: job.hostedUrl,
        atsPlatformName: 'Lever',
        scrapedTimestamp: new Date().toISOString(),
        description: parts.filter(Boolean).join('\n\n'),
        location,
        postedAt: job.createdAt
          ? new Date(job.createdAt).toISOString().slice(0, 10)
          : '',
      });
    },
  });
}
