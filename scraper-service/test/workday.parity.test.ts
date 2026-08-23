/**
 * Golden: native TS workday scraper vs fixtures/workday/expected.json. The list
 * endpoint returns the same postings for every search term (so cross-term dedupe
 * is exercised); details are served per externalPath, with one posting missing a
 * detail (404 -> list-location fallback) and one missing externalPath entirely.
 * Regenerate with UPDATE_GOLDEN=1.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';
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

test('workday: term mode paginates past a full first page', async () => {
  const PAGE = 20;
  const posting = (n: number) => ({
    title: `DevOps Engineer ${n}`,
    externalPath: `/job/devops-${n}`,
    locationsText: 'Remote',
  });
  // 23 matches for "devops": a full page at offset 0, a 3-post page at 20.
  const pages: Record<number, unknown[]> = {
    0: Array.from({ length: PAGE }, (_, i) => posting(i)),
    [PAGE]: [posting(PAGE), posting(PAGE + 1), posting(PAGE + 2)],
  };
  const offsetsByTerm: Record<string, number[]> = {};

  const restore = installFetchMock((url, init) => {
    if (url.endsWith('/AcmeCareers/jobs')) {
      const { searchText, offset } = JSON.parse(String(init?.body)) as {
        searchText: string;
        offset: number;
      };
      (offsetsByTerm[searchText] ||= []).push(offset);
      if (searchText !== 'devops') return { jobPostings: [] };
      return { jobPostings: pages[offset] || [] };
    }
    return undefined; // detail fetch -> 404 -> list-location fallback
  });
  try {
    const { scrapeWorkday: tsScrape } =
      await import('../src/scrapers/workday.js');
    const tsJobs = await tsScrape();
    assert.equal(tsJobs.length, PAGE + 3);
    // The full first page triggers exactly one follow-up; the short second
    // page ends the loop. Terms with a short first page stop immediately.
    assert.deepEqual(offsetsByTerm['devops'], [0, PAGE]);
    assert.deepEqual(offsetsByTerm['platform engineer'], [0]);
    assert.deepEqual(offsetsByTerm['sre'], [0]);
  } finally {
    restore();
  }
});
