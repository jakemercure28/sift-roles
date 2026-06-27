/**
 * Typed JobLead constructor + job-id derivation, ported from lib/job-lead.js.
 * makeJobLead trims the same 8 contract fields with the same `String(x || '')`
 * coercion; deriveJobId (and its slug/normalizeUrl helpers) reproduce the CJS
 * logic verbatim. Both run on the same Node runtime as the CJS original — same
 * WHATWG `URL` and `node:crypto` sha256 — so output is byte-identical, which the
 * parity tests assert against lib/job-lead.js.
 */
import { createHash } from 'node:crypto';

import type { JobLead } from '../types.js';

export interface MakeJobLeadInput {
  title?: string;
  company?: string;
  description?: string;
  directApplyUrl?: string;
  atsPlatformName?: string;
  scrapedTimestamp?: string;
  location?: string;
  postedAt?: string;
}

export function makeJobLead(input: MakeJobLeadInput): JobLead {
  const {
    title,
    company,
    description,
    directApplyUrl,
    atsPlatformName,
    scrapedTimestamp = new Date().toISOString(),
    location = '',
    postedAt = '',
  } = input;
  return {
    title: String(title || '').trim(),
    company: String(company || '').trim(),
    description: String(description || '').trim(),
    direct_apply_url: String(directApplyUrl || '').trim(),
    ats_platform_name: String(atsPlatformName || '').trim(),
    scraped_timestamp: scrapedTimestamp,
    location: String(location || '').trim(),
    posted_at: String(postedAt || '').trim(),
  };
}

/** Lowercase alphanumeric slug, max 40 chars; ported from lib/job-lead.js. */
function slug(value: unknown): string {
  return (
    String(value || '')
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, '-')
      .replace(/^-+|-+$/g, '')
      .slice(0, 40) || 'job'
  );
}

/** Drop the URL fragment; fall back to the trimmed input on parse failure. */
function normalizeUrl(value: unknown): string {
  try {
    const url = new URL(String(value || '').trim());
    url.hash = '';
    return url.toString();
  } catch {
    return String(value || '').trim();
  }
}

/** A lead or job carrying the fields deriveJobId reads. */
export interface JobIdInput {
  direct_apply_url?: string;
  url?: string;
  ats_platform_name?: string;
  platform?: string;
}

/**
 * Stable internal id for a lead/job, ported verbatim from lib/job-lead.js. ATS
 * URLs map to a platform-prefixed id (greenhouse-/ashby-/lever-/workday-);
 * anything else falls back to a platform slug + a sha256 of `platform|url`.
 */
export function deriveJobId(leadOrJob: JobIdInput): string {
  const url = normalizeUrl(leadOrJob.direct_apply_url || leadOrJob.url || '');
  const platform = leadOrJob.ats_platform_name || leadOrJob.platform || 'job';
  const platformSlug = slug(platform);

  try {
    const parsed = new URL(url);
    const host = parsed.hostname.toLowerCase();
    const parts = parsed.pathname.split('/').filter(Boolean);

    if (host.includes('greenhouse.io')) {
      const jobIndex = parts.findIndex((part) => part === 'jobs');
      const id = jobIndex >= 0 ? parts[jobIndex + 1] : '';
      if (id) return `greenhouse-${id}`;
    }

    if (host === 'jobs.ashbyhq.com' && parts[1]) {
      return `ashby-${parts[1]}`;
    }

    if (host === 'jobs.lever.co' && parts[1]) {
      return `lever-${parts[1]}`;
    }

    if (host.includes('myworkdayjobs.com')) {
      const id = parts[parts.length - 1];
      if (id) return `workday-${slug(host)}-${slug(id)}`;
    }
  } catch {
    // fall through to the hash-based id below
  }

  const hash = createHash('sha256')
    .update(`${platformSlug}|${url}`)
    .digest('hex')
    .slice(0, 16);
  return `${platformSlug}-${hash}`;
}
