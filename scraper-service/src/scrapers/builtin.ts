/**
 * Native TS port of scrapers/builtin.js. Built In has no API: each search term
 * fetches an HTML results page, job URLs are pulled by regex, and each job page
 * is parsed for its embedded JSON-LD JobPosting (plus the apply URL from the
 * inline Builtin.jobPostInit call). Verified byte-identical by
 * test/builtin.parity.test.ts.
 */
import { sleep, safeFetch } from '../lib/http.js';
import { stripHtml } from '../lib/html.js';
import { matchesSearchTerms } from '../lib/search.js';
import { companyConfig, SCRAPER_DELAY_MS } from '../lib/config.js';
import { makeJobLead } from '../lib/joblead.js';
import type { JobLead } from '../types.js';

// Built In has regional subdomains (e.g. builtinseattle.com, builtinnyc.com, builtinaustin.com).
// Set BUILTIN_SUBDOMAIN in .env to target a specific region, or leave default for nationwide.
const BUILTIN_SUBDOMAIN = process.env.BUILTIN_SUBDOMAIN || 'www';
const BASE_URL = `https://${BUILTIN_SUBDOMAIN}.builtin.com`;
const USER_AGENT =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36';

interface LdAddress {
  addressLocality?: string;
  addressRegion?: string;
  addressCountry?: string;
}

interface LdLocation {
  address?: LdAddress;
}

interface LdReq {
  name?: string;
}

interface JobPosting {
  '@type'?: string;
  title?: string;
  hiringOrganization?: { name?: string };
  baseSalary?: { value?: { minValue?: number; maxValue?: number } };
  jobLocation?: LdLocation | LdLocation[];
  jobLocationType?: string;
  applicantLocationRequirements?: LdReq | LdReq[];
  description?: string;
}

function extractJobUrls(html: string): string[] {
  const matches = [...html.matchAll(/href="(\/job\/[^"]+\/\d+)"/g)];
  const seen = new Set<string>();
  const urls: string[] = [];
  for (const m of matches) {
    const path = m[1];
    if (path && !seen.has(path)) {
      seen.add(path);
      urls.push(BASE_URL + path);
    }
  }
  return urls;
}

function parseJobPage(html: string, pageUrl: string): JobLead | null {
  // Parse LD+JSON structured data
  const ldMatch = html.match(/<script[^>]+ld[^>]+>([\s\S]*?)<\/script>/);
  if (!ldMatch) return null;

  let data: Record<string, unknown>;
  try {
    // unescape HTML entities in the script tag content
    const raw = (ldMatch[1] ?? '')
      .replace(/&#x2B;/g, '+')
      .replace(/&amp;/g, '&')
      .replace(/&lt;/g, '<')
      .replace(/&gt;/g, '>')
      .replace(/&#39;/g, "'")
      .replace(/&quot;/g, '"');
    data = JSON.parse(raw);
  } catch {
    return null;
  }

  const graph = (data['@graph'] as JobPosting[] | undefined) || [
    data as JobPosting,
  ];
  const posting = graph.find((item) => item['@type'] === 'JobPosting');
  if (!posting) return null;

  const org = posting.hiringOrganization || {};
  const salary = posting.baseSalary?.value || {};
  const jobLocations: LdLocation[] = Array.isArray(posting.jobLocation)
    ? posting.jobLocation
    : posting.jobLocation
      ? [posting.jobLocation]
      : [];
  const locParts = jobLocations
    .map((loc) => {
      const addr = loc?.address || {};
      // Prefer city/state; fall back to country alone so national postings aren't dropped.
      return (
        [addr.addressLocality, addr.addressRegion].filter(Boolean).join(', ') ||
        addr.addressCountry ||
        ''
      );
    })
    .filter(Boolean);
  const isTelecommute = posting.jobLocationType === 'TELECOMMUTE';
  // Remote roles often carry their allowed area here instead of in jobLocation.
  const reqs: LdReq[] = Array.isArray(posting.applicantLocationRequirements)
    ? posting.applicantLocationRequirements
    : posting.applicantLocationRequirements
      ? [posting.applicantLocationRequirements]
      : [];
  const reqNames = reqs.map((r) => r?.name).filter(Boolean);
  let location = '';
  if (locParts.length) {
    location = locParts.join(' | ');
  } else if (isTelecommute) {
    location = reqNames.length ? `Remote - ${reqNames.join(', ')}` : 'Remote';
  } else if (reqNames.length) {
    location = reqNames.join(', ');
  }
  // Build salary string for description context
  let salaryStr = '';
  if (salary.minValue && salary.maxValue) {
    salaryStr = `$${Math.round(salary.minValue / 1000)}K–$${Math.round(salary.maxValue / 1000)}K`;
  }

  // Extract apply URL from jobPostInit
  const initMatch = html.match(
    /Builtin\.jobPostInit\(\{"job":\{"id":\d+[^}]*"howToApply":"([^"\\]*)"/
  );
  const applyUrl = initMatch
    ? (initMatch[1] ?? '').replace(/\\u0026/g, '&')
    : pageUrl;

  return makeJobLead({
    title: posting.title || '',
    company: org.name || '',
    directApplyUrl: applyUrl || pageUrl,
    atsPlatformName: 'Built In',
    scrapedTimestamp: new Date().toISOString(),
    description: `${salaryStr ? salaryStr + ' | ' : ''}${stripHtml(posting.description || '')}`,
    location,
  });
}

export async function scrapeBuiltin(): Promise<JobLead[]> {
  const jobs: JobLead[] = [];
  const seenUrls = new Set<string>();
  const headers = { 'User-Agent': USER_AGENT };

  // Query Built In with the resume-derived search terms (its /jobs?search= takes
  // one term per request), so non-tech profiles get relevant results, not devops.
  for (const term of companyConfig().SEARCH_TERMS) {
    const searchUrl = `${BASE_URL}/jobs?search=${encodeURIComponent(term)}`;
    const res = await safeFetch(searchUrl, { headers }, `builtin/${term}`);
    if (!res) {
      await sleep(SCRAPER_DELAY_MS);
      continue;
    }

    let html: string;
    try {
      html = await res.text();
    } catch {
      await sleep(SCRAPER_DELAY_MS);
      continue;
    }

    const jobUrls = extractJobUrls(html);

    for (const url of jobUrls) {
      if (seenUrls.has(url)) continue;
      seenUrls.add(url);

      // Quick title check from URL slug before fetching full page
      const slugTitle =
        url.split('/job/')[1]?.split('/')[0]?.replace(/-/g, ' ') || '';
      if (!matchesSearchTerms(slugTitle)) continue;

      await sleep(SCRAPER_DELAY_MS);
      const jobRes = await safeFetch(url, { headers }, `builtin/job`);
      if (!jobRes) continue;

      let jobHtml: string;
      try {
        jobHtml = await jobRes.text();
      } catch {
        continue;
      }

      const job = parseJobPage(jobHtml, url);
      if (!job) continue;
      if (!matchesSearchTerms(job.title)) continue;

      jobs.push(job);
    }

    await sleep(SCRAPER_DELAY_MS * 2);
  }

  return jobs;
}
