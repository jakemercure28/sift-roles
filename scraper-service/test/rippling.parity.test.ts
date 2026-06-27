/**
 * Golden: native TS rippling scraper vs fixtures/rippling/expected.json.
 * Exercises the __NEXT_DATA__ extraction, pagination (totalPages = 2), the
 * search-term filter (Office Manager dropped), per-posting detail fetch, location
 * merge/dedupe, and the remote fallback. Regenerate with UPDATE_GOLDEN=1.
 */
import { test } from 'node:test';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { readFileSync } from 'node:fs';

import { installFetchMock, assertGolden } from './parity/harness.js';

const here = dirname(fileURLToPath(import.meta.url));
const fixtures = resolve(here, 'fixtures', 'rippling');

process.env.DATA_DIR = resolve(fixtures, 'profile');

const read = (name: string) => readFileSync(resolve(fixtures, name), 'utf8');
const page0 = read('page0.html');
const page1 = read('page1.html');
const detailU1 = read('detail-u1.html');
const detailU3 = read('detail-u3.html');

test('rippling: native TS output matches committed golden', async () => {
  const restore = installFetchMock((url) => {
    if (url.endsWith('/acme/jobs/u1')) return detailU1;
    if (url.endsWith('/acme/jobs/u3')) return detailU3;
    if (url.includes('/acme/jobs?page=1')) return page1;
    if (url.endsWith('/acme/jobs')) return page0;
    return undefined;
  });
  try {
    const { scrapeRippling: tsScrape } =
      await import('../src/scrapers/rippling.js');
    const tsJobs = await tsScrape();
    assertGolden(tsJobs, resolve(fixtures, 'expected.json'), { minCount: 2 });
  } finally {
    restore();
  }
});
