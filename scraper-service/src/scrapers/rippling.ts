/**
 * Native TS port of scrapers/rippling.js. Rippling's ATS is a Next.js app, so the
 * data lives in the __NEXT_DATA__ JSON blob: the jobs list is the React Query
 * 'job-posts' entry (paginated), and each posting's detail is another page's
 * apiData.jobPost. Verified byte-identical by test/rippling.parity.test.ts.
 */
import { sleep, safeFetch } from '../lib/http.js';
import { stripHtml } from '../lib/html.js';
import { matchesSearchTerms } from '../lib/search.js';
import { companyConfig, MAX_DESCRIPTION_LENGTH } from '../lib/config.js';
import { makeJobLead } from '../lib/joblead.js';
import type { JobLead } from '../types.js';

type RipplingLocation = string | { name?: string; city?: string } | null;

interface RipplingItem {
  id?: string;
  name?: string;
  url?: string;
  locations?: RipplingLocation[];
  workplaceType?: string;
}

interface RipplingDetail {
  description?: { company?: string; role?: string };
  locations?: RipplingLocation[];
  workplaceType?: string;
  companyName?: string;
}

interface PageData {
  items?: RipplingItem[];
  totalPages?: number;
}

function parseNextData(html: string): Record<string, unknown> | null {
  const match = html.match(
    /<script id="__NEXT_DATA__" type="application\/json">([\s\S]*?)<\/script>/
  );
  if (!match) return null;
  try {
    return JSON.parse(match[1] ?? '');
  } catch {
    return null;
  }
}

async function fetchJobsPage(
  slug: string,
  page: number
): Promise<PageData | null> {
  const url =
    page === 0
      ? `https://ats.rippling.com/${slug}/jobs`
      : `https://ats.rippling.com/${slug}/jobs?page=${page}`;
  const res = await safeFetch(
    url,
    {
      headers: {
        Accept: 'text/html,application/xhtml+xml',
        'User-Agent': 'Mozilla/5.0',
      },
    },
    `rippling/${slug}/page${page}`
  );
  if (!res) return null;
  const html = await res.text();
  const data = parseNextData(html);
  if (!data) return null;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any -- deep Next.js blob
  const queries: any[] =
    (data as any)?.props?.pageProps?.dehydratedState?.queries || [];
  const jobsQuery = queries.find(
    (q) => Array.isArray(q.queryKey) && q.queryKey[2] === 'job-posts'
  );
  return (jobsQuery?.state?.data as PageData) || null;
}

async function fetchJobDetail(
  slug: string,
  uuid: string | undefined
): Promise<RipplingDetail | null> {
  const url = `https://ats.rippling.com/${slug}/jobs/${uuid}`;
  const res = await safeFetch(
    url,
    {
      headers: {
        Accept: 'text/html,application/xhtml+xml',
        'User-Agent': 'Mozilla/5.0',
      },
    },
    `rippling/${slug}/${uuid}`
  );
  if (!res) return null;
  const html = await res.text();
  const data = parseNextData(html);
  // eslint-disable-next-line @typescript-eslint/no-explicit-any -- deep Next.js blob
  return (
    ((data as any)?.props?.pageProps?.apiData?.jobPost as RipplingDetail) ||
    null
  );
}

export async function scrapeRippling(): Promise<JobLead[]> {
  const jobs: JobLead[] = [];

  for (const slug of companyConfig().RIPPLING_COMPANIES) {
    const pageData = await fetchJobsPage(slug, 0);
    await sleep(300);
    if (!pageData) continue;

    const allItems: RipplingItem[] = [...(pageData.items || [])];
    const totalPages = pageData.totalPages || 1;

    for (let page = 1; page < totalPages; page++) {
      const pd = await fetchJobsPage(slug, page);
      await sleep(300);
      if (pd) allItems.push(...(pd.items || []));
    }

    for (const item of allItems) {
      if (!matchesSearchTerms(item.name || '')) continue;

      const detail = await fetchJobDetail(slug, item.id);
      await sleep(300);

      const rawDesc = detail
        ? (detail.description?.company || '') +
          ' ' +
          (detail.description?.role || '')
        : '';
      const description = stripHtml(rawDesc).slice(0, MAX_DESCRIPTION_LENGTH);

      const rawLocs = [
        ...(Array.isArray(item.locations) ? item.locations : []),
        ...(Array.isArray(detail?.locations) ? detail.locations : []),
      ]
        .map((l) => (typeof l === 'string' ? l : l?.name || l?.city || ''))
        .filter(Boolean);
      const seenLoc = new Set<string>();
      const dedupedLocs = rawLocs.filter((l) => {
        const key = l.toLowerCase();
        if (seenLoc.has(key)) return false;
        seenLoc.add(key);
        return true;
      });
      const isRemoteJob =
        item.workplaceType === 'remote' || detail?.workplaceType === 'remote';
      const location = dedupedLocs.length
        ? dedupedLocs.join(' | ')
        : isRemoteJob
          ? 'Remote'
          : '';

      jobs.push(
        makeJobLead({
          title: item.name,
          company: detail?.companyName || slug,
          directApplyUrl:
            item.url || `https://ats.rippling.com/${slug}/jobs/${item.id}`,
          atsPlatformName: 'Rippling',
          scrapedTimestamp: new Date().toISOString(),
          description,
          location,
        })
      );
    }
  }

  return jobs;
}
