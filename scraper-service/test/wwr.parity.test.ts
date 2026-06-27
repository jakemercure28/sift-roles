/**
 * Golden: native TS wwr scraper vs fixtures/wwr/expected.json. Serves two
 * committed RSS feeds (text bodies) covering CDATA + plain tags, the
 * "Company: Title" split, a non-matching item, and an item missing its link.
 * Regenerate with UPDATE_GOLDEN=1.
 */
import { test } from 'node:test';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { readFileSync } from 'node:fs';

import { installFetchMock, assertGolden } from './parity/harness.js';

const here = dirname(fileURLToPath(import.meta.url));
const fixtures = resolve(here, 'fixtures', 'wwr');

process.env.DATA_DIR = resolve(fixtures, 'profile');

const devops = readFileSync(resolve(fixtures, 'devops.rss'), 'utf8');
const fullstack = readFileSync(resolve(fixtures, 'fullstack.rss'), 'utf8');

test('wwr: native TS output matches committed golden', async () => {
  const restore = installFetchMock((url) => {
    if (url.includes('devops-sysadmin')) return devops;
    if (url.includes('full-stack')) return fullstack;
    return undefined;
  });
  try {
    const { scrapeWWR: tsScrape } = await import('../src/scrapers/wwr.js');
    const tsJobs = await tsScrape();
    assertGolden(tsJobs, resolve(fixtures, 'expected.json'), { minCount: 3 });
  } finally {
    restore();
  }
});
