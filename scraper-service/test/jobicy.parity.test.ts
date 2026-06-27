/**
 * Golden: native TS jobicy scraper vs fixtures/jobicy/expected.json, against a
 * committed single-endpoint Remotive fixture. Regenerate with UPDATE_GOLDEN=1.
 */
import { test } from 'node:test';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { readFileSync } from 'node:fs';

import { installFetchMock, assertGolden } from './parity/harness.js';

const here = dirname(fileURLToPath(import.meta.url));
const fixtures = resolve(here, 'fixtures', 'jobicy');

process.env.DATA_DIR = resolve(fixtures, 'profile');

const response = JSON.parse(
  readFileSync(resolve(fixtures, 'response.json'), 'utf8')
);

test('jobicy: native TS output matches committed golden', async () => {
  const restore = installFetchMock((url) =>
    url.includes('category=devops-sysadmin') ? response : undefined
  );
  try {
    const { scrapeJobicy: tsScrape } =
      await import('../src/scrapers/jobicy.js');
    const tsJobs = await tsScrape();
    assertGolden(tsJobs, resolve(fixtures, 'expected.json'), { minCount: 2 });
  } finally {
    restore();
  }
});
