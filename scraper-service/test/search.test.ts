/**
 * Golden snapshot for matchesSearchTerms. This used to diff against the
 * CommonJS lib/scraper-utils.js; that file was retired once the TS port was
 * confirmed byte-identical, so fixtures/search/results.json is now the source of
 * truth. Covers multi-word, long-single-word substring, and short-word boundary
 * cases. Regenerate with UPDATE_GOLDEN=1.
 */
import { test } from 'node:test';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

import { assertJsonGolden } from './parity/harness.js';

const here = dirname(fileURLToPath(import.meta.url));
const fixtures = resolve(here, 'fixtures', 'search');

const TERMS = ['devops', 'platform engineer', 'sre', 'vp', 'tax', 'investment'];

const TEXTS = [
  'Senior DevOps Engineer',
  'Platform Engineer',
  'platformengineer',
  'SRE Lead',
  'syntax wizard',
  'VP of Sales',
  'Developer (no match)',
  'Tax Associate',
  'Investments Manager',
  '',
  'staff sre / platform',
];

test('matchesSearchTerms matches committed golden', async () => {
  const { matchesSearchTerms } = await import('../src/lib/search.js');
  const out = TEXTS.map((text) => ({
    text,
    matched: matchesSearchTerms(text, TERMS),
  }));
  assertJsonGolden(out, resolve(fixtures, 'results.json'));
});
