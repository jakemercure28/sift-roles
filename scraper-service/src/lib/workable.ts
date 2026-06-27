/**
 * Native TS port of lib/workable.js. Workable has no single stable public API,
 * so we try three endpoints in order (public-account -> widget-account ->
 * v3-account-jobs) and use the first that responds OK. The response shape varies,
 * so jobs are found by a recursive search for arrays of job-like objects, deduped
 * by shortcode/code/id. Verified byte-identical to the JS original via the
 * workable parity test.
 */
import { stripHtml } from './html.js';
import { makeJobLead } from './joblead.js';
import type { JobLead } from '../types.js';

interface WorkableEndpoint {
  name: string;
  url: (slug: string) => string;
  options: (slug?: string) => RequestInit;
}

export const WORKABLE_ENDPOINTS: WorkableEndpoint[] = [
  {
    name: 'public-account',
    url: (slug) => `https://www.workable.com/api/accounts/${slug}?details=true`,
    options: () => ({ headers: { Accept: 'application/json' } }),
  },
  {
    name: 'widget-account',
    url: (slug) => `https://apply.workable.com/api/v1/widget/accounts/${slug}`,
    options: () => ({ headers: { Accept: 'application/json' } }),
  },
  {
    name: 'v3-account-jobs',
    url: (slug) => `https://apply.workable.com/api/v3/accounts/${slug}/jobs`,
    options: () => ({
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Accept: 'application/json',
      },
      body: JSON.stringify({
        query: '',
        location: [],
        department: [],
        worktype: [],
        remote: [],
      }),
    }),
  },
];

interface WorkableJobRaw {
  title?: string;
  full_title?: string;
  shortcode?: string;
  code?: string;
  id?: string | number;
  url?: string;
  application_url?: string;
  apply_url?: string;
  description?: string;
  description_html?: string;
  full_description?: string;
  requirements?: string;
  location?: {
    city?: string;
    region?: string;
    country?: string;
    workplace_type?: string;
  };
  city?: string;
  state?: string;
  country?: string;
  remote?: boolean;
  telecommuting?: boolean;
  company?: string;
  created_at?: string;
}

function looksLikeWorkableJob(value: unknown): value is WorkableJobRaw {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const v = value as WorkableJobRaw;
  return Boolean(
    (v.title || v.full_title) &&
    (v.shortcode || v.code || v.id || v.url || v.application_url)
  );
}

function collectJobObjects(
  value: unknown,
  jobs: WorkableJobRaw[] = [],
  seen = new Set<unknown>()
): WorkableJobRaw[] {
  if (!value || typeof value !== 'object') return jobs;
  if (Array.isArray(value)) {
    if (value.some(looksLikeWorkableJob)) {
      for (const item of value) {
        if (!looksLikeWorkableJob(item)) continue;
        const key =
          item.shortcode ||
          item.code ||
          item.id ||
          item.url ||
          JSON.stringify(item);
        if (seen.has(key)) continue;
        seen.add(key);
        jobs.push(item);
      }
      return jobs;
    }
    for (const item of value) collectJobObjects(item, jobs, seen);
    return jobs;
  }

  for (const nested of Object.values(value as Record<string, unknown>))
    collectJobObjects(nested, jobs, seen);
  return jobs;
}

function accountName(data: unknown, slug: string): string {
  const d = data as
    | {
        name?: string;
        company?: { name?: string };
        account?: { name?: string; company_name?: string };
      }
    | null
    | undefined;
  return (
    d?.name ||
    d?.company?.name ||
    d?.account?.name ||
    d?.account?.company_name ||
    slug
  );
}

export function normalizeWorkableJobs(data: unknown, slug: string): JobLead[] {
  const company = accountName(data, slug);
  return collectJobObjects(data).map((job) => {
    const shortcode = job.shortcode || job.code || job.id;
    const url =
      job.url ||
      job.application_url ||
      job.apply_url ||
      (shortcode
        ? `https://apply.workable.com/j/${shortcode}`
        : `https://apply.workable.com/${slug}`);
    const description = [
      job.description,
      job.description_html,
      job.full_description,
      job.requirements,
    ]
      .filter(Boolean)
      .join('\n\n');

    const locationParts = [
      job.location?.city || job.city,
      job.location?.region || job.state,
      job.location?.country || job.country,
    ].filter(Boolean);
    const isRemoteJob =
      job.remote ||
      job.telecommuting ||
      job.location?.workplace_type === 'remote';
    const location = locationParts.length
      ? locationParts.join(', ')
      : isRemoteJob
        ? 'Remote'
        : '';
    return makeJobLead({
      title: job.title || job.full_title || '',
      company: job.company || company,
      directApplyUrl: url,
      atsPlatformName: 'Workable',
      scrapedTimestamp: new Date().toISOString(),
      description: stripHtml(description || ''),
      location,
      postedAt: job.created_at || '',
    });
  });
}

interface FetchResult {
  ok: boolean;
  status: number;
  url: string;
  data: unknown;
  error?: string;
}

async function fetchJsonWithStatus(
  url: string,
  options: RequestInit,
  fetchImpl: typeof fetch = fetch
): Promise<FetchResult> {
  try {
    const res = await fetchImpl(url, {
      signal: AbortSignal.timeout(12_000),
      ...options,
    });
    let data: unknown = null;
    if (res && res.ok) {
      try {
        data = await res.json();
      } catch {
        // non-JSON body: leave data null, treat as empty
      }
    }
    return {
      ok: Boolean(res?.ok),
      status: res?.status || 0,
      url: res?.url || url,
      data,
    };
  } catch (e) {
    return {
      ok: false,
      status: 0,
      error: e instanceof Error ? e.message : String(e),
      url,
      data: null,
    };
  }
}

interface Attempt {
  endpoint: string;
  url: string;
  status: number;
  ok: boolean;
  count: number;
  error: string | null;
}

export interface WorkableResult {
  result: 'ok' | 'empty' | 'blocked' | 'broken';
  count: number;
  jobs: JobLead[];
  note?: string;
  attempts: Attempt[];
}

export async function fetchWorkableAccountJobs(
  slug: string,
  { fetch: fetchImpl = fetch }: { fetch?: typeof fetch } = {}
): Promise<WorkableResult> {
  const attempts: Attempt[] = [];

  for (const endpoint of WORKABLE_ENDPOINTS) {
    const url = endpoint.url(slug);
    const result = await fetchJsonWithStatus(
      url,
      endpoint.options(slug),
      fetchImpl
    );
    const jobs = result.ok ? normalizeWorkableJobs(result.data, slug) : [];
    attempts.push({
      endpoint: endpoint.name,
      url,
      status: result.status,
      ok: result.ok,
      count: jobs.length,
      error: result.error || null,
    });

    if (result.ok) {
      return {
        result: jobs.length > 0 ? 'ok' : 'empty',
        count: jobs.length,
        jobs,
        attempts,
      };
    }
  }

  const blocked = attempts.find((attempt) => attempt.status === 429);
  if (blocked) {
    return {
      result: 'blocked',
      count: 0,
      jobs: [],
      note: `HTTP 429 at ${blocked.endpoint}`,
      attempts,
    };
  }

  const last = attempts[attempts.length - 1];
  return {
    result: 'broken',
    count: 0,
    jobs: [],
    note: `HTTP ${last?.status || last?.error || 0}`,
    attempts,
  };
}
