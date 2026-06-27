/**
 * Golden: native TS builtin scraper vs fixtures/builtin/expected.json. Serves an
 * HTML search page (with a duplicate link + a non-matching slug that is
 * pre-filtered) and two job pages: one with full JSON-LD + salary + jobPostInit
 * apply URL, one remote (TELECOMMUTE) posting with no salary and apply-URL
 * fallback to the page URL. Regenerate with UPDATE_GOLDEN=1.
 */
import { test } from 'node:test';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { readFileSync } from 'node:fs';

import { installFetchMock, assertGolden } from './parity/harness.js';

const here = dirname(fileURLToPath(import.meta.url));
const fixtures = resolve(here, 'fixtures', 'builtin');

process.env.DATA_DIR = resolve(fixtures, 'profile');

const search = readFileSync(resolve(fixtures, 'search.html'), 'utf8');
const job1001 = readFileSync(resolve(fixtures, 'job-1001.html'), 'utf8');
const job1002 = readFileSync(resolve(fixtures, 'job-1002.html'), 'utf8');

test('builtin: native TS output matches committed golden', async () => {
  const restore = installFetchMock((url) => {
    if (url.includes('/jobs?search=')) return search;
    if (url.endsWith('/1001')) return job1001;
    if (url.endsWith('/1002')) return job1002;
    return undefined;
  });
  try {
    const { scrapeBuiltin: tsScrape } =
      await import('../src/scrapers/builtin.js');
    const tsJobs = await tsScrape();
    assertGolden(tsJobs, resolve(fixtures, 'expected.json'), { minCount: 2 });
  } finally {
    restore();
  }
});
