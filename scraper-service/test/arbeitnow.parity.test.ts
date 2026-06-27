/**
 * Golden: native TS arbeitnow scraper vs fixtures/arbeitnow/expected.json,
 * against a committed single-endpoint fixture (array + empty-array location
 * cases). Regenerate with UPDATE_GOLDEN=1.
 */
import { test } from 'node:test';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { readFileSync } from 'node:fs';

import { installFetchMock, assertGolden } from './parity/harness.js';

const here = dirname(fileURLToPath(import.meta.url));
const fixtures = resolve(here, 'fixtures', 'arbeitnow');

process.env.DATA_DIR = resolve(fixtures, 'profile');

const response = JSON.parse(
  readFileSync(resolve(fixtures, 'response.json'), 'utf8')
);

test('arbeitnow: native TS output matches committed golden', async () => {
  const restore = installFetchMock((url) =>
    url.includes('arbeitnow.com/api/job-board-api') ? response : undefined
  );
  try {
    const { scrapeArbeitnow: tsScrape } =
      await import('../src/scrapers/arbeitnow.js');
    const tsJobs = await tsScrape();
    assertGolden(tsJobs, resolve(fixtures, 'expected.json'), { minCount: 2 });
  } finally {
    restore();
  }
});
