/**
 * Golden: native TS remoteok scraper vs fixtures/remoteok/expected.json, against
 * a committed single-endpoint fixture (incl. the legal-notice element at index
 * 0). Regenerate with UPDATE_GOLDEN=1.
 */
import { test } from 'node:test';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { readFileSync } from 'node:fs';

import { installFetchMock, assertGolden } from './parity/harness.js';

const here = dirname(fileURLToPath(import.meta.url));
const fixtures = resolve(here, 'fixtures', 'remoteok');

process.env.DATA_DIR = resolve(fixtures, 'profile');

const response = JSON.parse(
  readFileSync(resolve(fixtures, 'response.json'), 'utf8')
);

test('remoteok: native TS output matches committed golden', async () => {
  const restore = installFetchMock((url) =>
    url.startsWith('https://remoteok.com/api') ? response : undefined
  );
  try {
    const { scrapeRemoteOK: tsScrape } =
      await import('../src/scrapers/remoteok.js');
    const tsJobs = await tsScrape();
    assertGolden(tsJobs, resolve(fixtures, 'expected.json'), { minCount: 2 });
  } finally {
    restore();
  }
});
