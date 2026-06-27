/**
 * Native TS port of scrapers/ashby.js. Ashby's public posting API exposes
 * optional compensation (prepended to the description) and a layered location
 * (locationName -> location -> structured postalAddress -> Remote, plus
 * secondaryLocations). Verified byte-identical by test/ashby.parity.test.ts.
 */
import { stripHtml } from '../lib/html.js';
import { scrapeCompanies } from '../lib/companies.js';
import { makeJobLead } from '../lib/joblead.js';
import { companyConfig, MAX_DESCRIPTION_LENGTH } from '../lib/config.js';
import type { JobLead } from '../types.js';

interface AshbyLocation {
  location?: string;
  locationName?: string;
}

interface AshbyJob {
  title?: string;
  descriptionPlain?: string;
  descriptionHtml?: string;
  compensation?: {
    scrapeableCompensationSalarySummary?: string;
    compensationTierSummary?: string;
  };
  address?: {
    postalAddress?: {
      addressLocality?: string;
      addressRegion?: string;
      addressCountry?: string;
    };
  };
  locationName?: string;
  location?: string;
  workplaceType?: string;
  secondaryLocations?: Array<AshbyLocation | null>;
  companyName?: string;
  jobUrl?: string;
  updatedAt?: string;
  publishedAt?: string;
}

export async function scrapeAshby(): Promise<JobLead[]> {
  return scrapeCompanies<AshbyJob>({
    companies: companyConfig().ASHBY_COMPANIES,
    platform: 'ashby',
    buildUrl: (company) =>
      `https://api.ashbyhq.com/posting-api/job-board/${company}?includeCompensation=true`,
    parseResponse: (data) => (data as { jobs?: AshbyJob[] }).jobs || [],
    matchField: (job) => job.title || '',
    mapJob: (job, company) => {
      const baseDesc =
        job.descriptionPlain || stripHtml(job.descriptionHtml || '');
      const salarySummary =
        job.compensation?.scrapeableCompensationSalarySummary ||
        job.compensation?.compensationTierSummary ||
        '';
      const description = (
        salarySummary
          ? `Compensation: ${salarySummary}\n\n${baseDesc}`
          : baseDesc
      ).slice(0, MAX_DESCRIPTION_LENGTH);
      // Ashby's public posting API leaves `locationName` null; the real value lives in
      // `job.location` (a string) or the structured `job.address.postalAddress`.
      const addr: {
        addressLocality?: string;
        addressRegion?: string;
        addressCountry?: string;
      } = job.address?.postalAddress || {};
      const addressLoc = [
        addr.addressLocality,
        addr.addressRegion,
        addr.addressCountry,
      ]
        .filter(Boolean)
        .join(', ');
      const primary =
        job.locationName ||
        job.location ||
        addressLoc ||
        (job.workplaceType === 'Remote' ? 'Remote' : '');
      const secondary = Array.isArray(job.secondaryLocations)
        ? job.secondaryLocations
            .map((l) => l?.location || l?.locationName)
            .filter(Boolean)
        : [];
      const location = [primary, ...secondary].filter(Boolean).join(' | ');
      return makeJobLead({
        title: job.title,
        company: job.companyName || company,
        directApplyUrl: job.jobUrl,
        atsPlatformName: 'Ashby',
        scrapedTimestamp: new Date().toISOString(),
        description,
        location,
        postedAt: job.updatedAt || job.publishedAt || '',
      });
    },
  });
}
