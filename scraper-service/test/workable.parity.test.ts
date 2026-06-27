/**
 * Golden: native TS workable scraper vs fixtures/workable/expected.json.
 * Exercises the endpoint fallback: acme answers on the public-account endpoint,
 * while globex 404s there and is served by the widget-account endpoint.
 * Regenerate with UPDATE_GOLDEN=1.
 */
import { test } from 'node:test';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { readFileSync } from 'node:fs';

import { installFetchMock, assertGolden } from './parity/harness.js';

const here = dirname(fileURLToPath(import.meta.url));
const fixtures = resolve(here, 'fixtures', 'workable');

process.env.DATA_DIR = resolve(fixtures, 'profile');

const acmePublic = JSON.parse(
  readFileSync(resolve(fixtures, 'acme.public.json'), 'utf8')
);
const globexWidget = JSON.parse(
  readFileSync(resolve(fixtures, 'globex.widget.json'), 'utf8')
);

test('workable: native TS output matches committed golden', async () => {
  const restore = installFetchMock((url) => {
    if (url.includes('/api/accounts/acme')) return acmePublic;
    if (url.includes('/widget/accounts/globex')) return globexWidget;
    return undefined; // everything else (incl. globex public-account) 404s
  });
  try {
    const { scrapeWorkable: tsScrape } =
      await import('../src/scrapers/workable.js');
    const tsJobs = await tsScrape();
    assertGolden(tsJobs, resolve(fixtures, 'expected.json'), { minCount: 3 });
  } finally {
    restore();
  }
});
