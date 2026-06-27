/**
 * Golden: native TS workday scraper vs fixtures/workday/expected.json. The list
 * endpoint returns the same postings for every search term (so cross-term dedupe
 * is exercised); details are served per externalPath, with one posting missing a
 * detail (404 -> list-location fallback) and one missing externalPath entirely.
 * Regenerate with UPDATE_GOLDEN=1.
 */
import { test } from 'node:test';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { readFileSync } from 'node:fs';

import { installFetchMock, assertGolden } from './parity/harness.js';

const here = dirname(fileURLToPath(import.meta.url));
const fixtures = resolve(here, 'fixtures', 'workday');

process.env.DATA_DIR = resolve(fixtures, 'profile');

const list = JSON.parse(readFileSync(resolve(fixtures, 'list.json'), 'utf8'));
const details = JSON.parse(
  readFileSync(resolve(fixtures, 'details.json'), 'utf8')
) as Record<string, unknown>;

test('workday: native TS output matches committed golden', async () => {
  const restore = installFetchMock((url) => {
    if (url.endsWith('/AcmeCareers/jobs')) return list;
    for (const path of Object.keys(details)) {
      if (url.endsWith(`/AcmeCareers${path}`)) return details[path];
    }
    return undefined; // unknown detail path -> 404 -> list-location fallback
  });
  try {
    const { scrapeWorkday: tsScrape } =
      await import('../src/scrapers/workday.js');
    const tsJobs = await tsScrape();
    assertGolden(tsJobs, resolve(fixtures, 'expected.json'), { minCount: 4 });
  } finally {
    restore();
  }
});
