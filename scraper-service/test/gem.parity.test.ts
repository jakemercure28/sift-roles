/**
 * Golden: the native TS gem scraper must emit the committed JobLeads in
 * fixtures/gem/expected.json. Gem POSTs list and detail to the same GraphQL
 * URL, so the mock routes on the request body's operationName (and extId for
 * details). Term mode keeps title matches; board mode returns every posting.
 * Regenerate with UPDATE_GOLDEN=1.
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
const fixtures = resolve(here, 'fixtures', 'gem');

// Point the scraper at the fixture company list / search terms.
process.env.DATA_DIR = resolve(fixtures, 'profile');

const responses = JSON.parse(
  readFileSync(resolve(fixtures, 'responses.json'), 'utf8')
) as { list: unknown; details: Record<string, unknown> };

/** Route a Gem GraphQL POST to the committed list/detail fixtures. */
function gemMock(_url: string, init?: RequestInit): unknown {
  const body = JSON.parse(String(init?.body ?? '{}')) as {
    operationName?: string;
    variables?: { extId?: string };
  };
  if (body.operationName === 'JobBoardList') return responses.list;
  if (body.operationName === 'ExternalJobPostingQuery') {
    const extId = body.variables?.extId ?? '';
    return (
      responses.details[extId] ?? { data: { oatsExternalJobPosting: null } }
    );
  }
  return undefined;
}

test('gem: native TS term-mode output matches committed golden', async () => {
  const restore = installFetchMock(gemMock);
  try {
    const { scrapeGem } = await import('../src/scrapers/gem.js');
    const jobs = await scrapeGem();
    assertGolden(jobs, resolve(fixtures, 'expected.json'), { minCount: 2 });
  } finally {
    restore();
  }
});

test('gem: board mode returns every posting with an ok result', async () => {
  const restore = installFetchMock(gemMock);
  try {
    const { scrapeGemBoards } = await import('../src/scrapers/gem.js');
    const { leads, results } = await scrapeGemBoards(['acme']);
    // No search-term filter: all three postings become leads.
    assert.equal(normalizeLeads(leads).length, 3);
    assert.deepEqual(results, [
      { platform: 'gem', slug: 'acme', company: 'Acme', status: 'ok' },
    ]);
    // The board label, not the slug, is stamped on each lead.
    assert.ok(leads.every((l) => l.company === 'Acme'));
  } finally {
    restore();
  }
});
