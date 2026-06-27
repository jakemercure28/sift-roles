/**
 * JobLead contract validation, ported natively from the former root lib/validate.js
 * (the single source of truth now lives here in TS). Every scraper's output passes
 * through validateJobs in bridge.ts: malformed leads are dropped, valid ones are
 * returned trimmed to the strict 8-field JobLead shape.
 */
import { CONTRACT_FIELDS } from '../types.js';
import type { JobLead } from '../types.js';
import { warn } from './log.js';

// Required fields are every contract field except the two optional ones.
const REQUIRED_FIELDS = CONTRACT_FIELDS.filter(
  (f) => f !== 'location' && f !== 'posted_at'
);

/** Validate one candidate; returns the trimmed JobLead or null if it fails the contract. */
export function validateJob(job: unknown): JobLead | null {
  if (!job || typeof job !== 'object') return null;
  const rec = job as Record<string, unknown>;
  const keys = Object.keys(rec);
  if (keys.some((field) => !CONTRACT_FIELDS.includes(field as keyof JobLead))) {
    return null;
  }
  for (const field of REQUIRED_FIELDS) {
    const value = rec[field];
    if (typeof value !== 'string' || !value.trim()) return null;
  }
  if (rec.location != null && typeof rec.location !== 'string') return null;
  if (rec.posted_at != null && typeof rec.posted_at !== 'string') return null;
  try {
    const url = new URL(rec.direct_apply_url as string);
    if (url.protocol !== 'http:' && url.protocol !== 'https:') return null;
  } catch {
    return null;
  }
  if (Number.isNaN(Date.parse(rec.scraped_timestamp as string))) return null;

  return {
    title: (rec.title as string).trim(),
    company: (rec.company as string).trim(),
    description: (rec.description as string).trim(),
    direct_apply_url: (rec.direct_apply_url as string).trim(),
    ats_platform_name: (rec.ats_platform_name as string).trim(),
    scraped_timestamp: rec.scraped_timestamp as string,
    location: ((rec.location as string) || '').trim(),
    posted_at: ((rec.posted_at as string) || '').trim(),
  };
}

/** Validate a batch, dropping malformed leads (logged once per source). */
export function validateJobs(jobs: unknown[], label: string): JobLead[] {
  const valid = jobs.map(validateJob).filter((j): j is JobLead => j !== null);
  const dropped = jobs.length - valid.length;
  if (dropped > 0) {
    warn('Dropped malformed jobs', { source: label, count: dropped });
  }
  return valid;
}
