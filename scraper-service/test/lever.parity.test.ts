/**
 * Golden: the native TS lever scraper must emit the committed JobLeads in
 * fixtures/lever/expected.json. It runs against the same committed fixtures and
 * fixture profile, so any difference is a real mapping change. Regenerate with
 * UPDATE_GOLDEN=1.
 */
import { test } from 'node:test';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { readFileSync } from 'node:fs';

import { installFetchMock, assertGolden } from './parity/harness.js';

const here = dirname(fileURLToPath(import.meta.url));
const fixtures = resolve(here, 'fixtures', 'lever');

// Point the scraper at the fixture company list / search terms.
process.env.DATA_DIR = resolve(fixtures, 'profile');

const responses = JSON.parse(
  readFileSync(resolve(fixtures, 'responses.json'), 'utf8')
) as Record<string, unknown[]>;

function companyFromUrl(url: string): string | undefined {
  const m = url.match(/\/postings\/([^/?]+)/);
  return m?.[1];
}

test('lever: native TS output matches committed golden', async () => {
  const restore = installFetchMock((url) => {
    const company = companyFromUrl(url);
    if (!company) return undefined;
    return responses[company] ?? [];
  });
  try {
    const { scrapeLever: tsScrape } = await import('../src/scrapers/lever.js');
    const tsJobs = await tsScrape();
    assertGolden(tsJobs, resolve(fixtures, 'expected.json'), { minCount: 3 });
  } finally {
    restore();
  }
});
