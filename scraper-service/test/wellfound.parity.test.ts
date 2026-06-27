/**
 * Golden: native TS wellfound scraper vs fixtures/wellfound/expected.json.
 * Remotive is queried once per search term; the fixture maps each term to a
 * response and exercises cross-term dedupe by job id. Regenerate with
 * UPDATE_GOLDEN=1.
 */
import { test } from 'node:test';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { readFileSync } from 'node:fs';

import { installFetchMock, assertGolden } from './parity/harness.js';

const here = dirname(fileURLToPath(import.meta.url));
const fixtures = resolve(here, 'fixtures', 'wellfound');

process.env.DATA_DIR = resolve(fixtures, 'profile');

const responses = JSON.parse(
  readFileSync(resolve(fixtures, 'responses.json'), 'utf8')
) as Record<string, { jobs: unknown[] }>;

function termFromUrl(url: string): string | null {
  try {
    return new URL(url).searchParams.get('search');
  } catch {
    return null;
  }
}

test('wellfound: native TS output matches committed golden', async () => {
  const restore = installFetchMock((url) => {
    const term = termFromUrl(url);
    if (!term) return undefined;
    return responses[term] ?? { jobs: [] };
  });
  try {
    const { scrapeWellfound: tsScrape } =
      await import('../src/scrapers/wellfound.js');
    const tsJobs = await tsScrape();
    assertGolden(tsJobs, resolve(fixtures, 'expected.json'), { minCount: 3 });
  } finally {
    restore();
  }
});
