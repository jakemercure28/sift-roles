/**
 * Incremental board fetching for board mode's single-request platforms
 * (greenhouse/lever/ashby: one GET per board). Each fetch sends the previous
 * cycle's ETag as If-None-Match and hashes the response body; a 304 or an
 * unchanged hash skips parse/map entirely and reports the board `unchanged`,
 * so a steady-state global pass only pays full parsing for boards that
 * actually moved. Platforms whose boards need several requests (workday,
 * workable) keep their own full-fetch paths.
 */
import { createHash } from 'node:crypto';

import { conditionalFetch, sleep, HostPacer } from './http.js';
import type { ScrapeCompaniesOptions } from './companies.js';
import { info } from './log.js';
import type { JobLead, BoardRef, BoardFetchResult } from '../types.js';

const BOARD_CONCURRENCY = 15;
const FETCH_RETRIES = 2;
const RETRY_BACKOFF_MS = 750;

export interface BoardScrapeOutcome {
  leads: JobLead[];
  results: BoardFetchResult[];
}

/**
 * Fetch the given boards through one platform's fetch/parse/map config,
 * skipping boards whose ETag or content hash shows no change since the state
 * carried on the BoardRef. Every posting is kept (board mode does no
 * search-term filtering).
 */
export async function scrapeBoardsConditional<Item>(
  opts: ScrapeCompaniesOptions<Item>,
  boards: BoardRef[],
  pacer: HostPacer
): Promise<BoardScrapeOutcome> {
  const leads: JobLead[] = [];
  const results: BoardFetchResult[] = [];
  let unchanged = 0;
  let failed = 0;

  async function fetchBoard(ref: BoardRef): Promise<void> {
    const url = opts.buildUrl(ref.slug);
    const label = `${opts.platform}/${ref.slug}`;
    const result: BoardFetchResult = {
      platform: opts.platform,
      slug: ref.slug,
      status: 'failed',
    };
    results.push(result);

    for (let attempt = 0; attempt <= FETCH_RETRIES; attempt++) {
      await pacer.wait(url);
      const { res, notModified } = await conditionalFetch(url, ref.etag, label);
      if (notModified) {
        result.status = 'unchanged';
        result.etag = ref.etag;
        result.contentHash = ref.contentHash;
        unchanged++;
        return;
      }
      if (!res) {
        if (attempt < FETCH_RETRIES)
          await sleep(RETRY_BACKOFF_MS * (attempt + 1));
        continue;
      }
      let text: string;
      try {
        text = await res.text();
      } catch {
        break;
      }
      const hash = createHash('sha256').update(text).digest('hex');
      result.etag = res.headers.get('etag') ?? undefined;
      result.contentHash = hash;
      // No ETag support (or a proxy stripped it): the body hash still detects
      // an unchanged board without re-parsing it.
      if (ref.contentHash && ref.contentHash === hash) {
        result.status = 'unchanged';
        unchanged++;
        return;
      }
      let data: unknown;
      try {
        data = JSON.parse(text);
      } catch {
        break;
      }
      const items = opts.parseResponse(data, ref.slug) || [];
      for (const item of items) {
        leads.push(opts.mapJob(item, ref.slug));
      }
      result.status = 'ok';
      return;
    }
    failed++;
  }

  for (let i = 0; i < boards.length; i += BOARD_CONCURRENCY) {
    const batch = boards.slice(i, i + BOARD_CONCURRENCY);
    await Promise.allSettled(batch.map(fetchBoard));
  }

  info('Board scrape stats', {
    platform: opts.platform,
    boards: boards.length,
    unchanged,
    failed,
    leads: leads.length,
  });
  return { leads, results };
}
