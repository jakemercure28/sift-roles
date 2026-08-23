/**
 * Golden: the native TS SmartRecruiters scraper must emit the committed
 * JobLeads in fixtures/smartrecruiters/expected.json. SmartRecruiters GETs a
 * paginated list then one detail per posting, so the mock routes on the URL
 * path: /postings serves the list, /postings/{id} serves that posting's detail.
 * Term mode keeps title matches; board mode returns every posting. Regenerate
 * with UPDATE_GOLDEN=1.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { readFileSync } from 'node:fs';

import {
  installFetchMock,
  assertGolden,
  normalizeLeads,
} from './parity/harness.js';

const here = dirname(fileURLToPath(import.meta.url));
const fixtures = resolve(here, 'fixtures', 'smartrecruiters');

// Point the scraper at the fixture company list / search terms.
process.env.DATA_DIR = resolve(fixtures, 'profile');

const responses = JSON.parse(
  readFileSync(resolve(fixtures, 'responses.json'), 'utf8')
) as { list: unknown; details: Record<string, unknown> };

/** Route a SmartRecruiters GET to the committed list/detail fixtures. */
function srMock(url: string): unknown {
  const { pathname } = new URL(url);
  const detail = pathname.match(/\/postings\/([^/]+)$/);
  if (detail) return responses.details[detail[1]] ?? {};
  if (pathname.endsWith('/postings')) return responses.list;
  return undefined;
}

test('smartrecruiters: native TS term-mode output matches committed golden', async () => {
  const restore = installFetchMock(srMock);
  try {
    const { scrapeSmartRecruiters } =
      await import('../src/scrapers/smartrecruiters.js');
    const jobs = await scrapeSmartRecruiters();
    assertGolden(jobs, resolve(fixtures, 'expected.json'), { minCount: 2 });
  } finally {
    restore();
  }
});

test('smartrecruiters: board mode returns every posting with an ok result', async () => {
  const restore = installFetchMock(srMock);
  try {
    const { scrapeSmartRecruitersBoards } =
      await import('../src/scrapers/smartrecruiters.js');
    const { leads, results } = await scrapeSmartRecruitersBoards(['acme']);
    // No search-term filter: all three postings become leads.
    assert.equal(normalizeLeads(leads).length, 3);
    assert.deepEqual(results, [
      {
        platform: 'smartrecruiters',
        slug: 'acme',
        company: 'Acme',
        status: 'ok',
      },
    ]);
    // The board label, not the slug, is stamped on each lead.
    assert.ok(leads.every((l) => l.company === 'Acme'));
  } finally {
    restore();
  }
});
