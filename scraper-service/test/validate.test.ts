/**
 * Native contract validation (src/lib/validate.ts). Ported from the former root
 * lib/validate.js behavior: only the strict 8 contract fields, required fields
 * non-blank, http(s) apply URL, parseable timestamp.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';

import { validateJob, validateJobs } from '../src/lib/validate.js';

const good = {
  title: '  Senior Engineer ',
  company: ' Acme ',
  description: ' build things ',
  direct_apply_url: ' https://example.com/job/1 ',
  ats_platform_name: ' Greenhouse ',
  scraped_timestamp: '2026-01-01T00:00:00.000Z',
  location: ' Remote ',
  posted_at: ' 2026-01-01 ',
};

test('trims a valid lead to the strict shape', () => {
  assert.deepEqual(validateJob(good), {
    title: 'Senior Engineer',
    company: 'Acme',
    description: 'build things',
    direct_apply_url: 'https://example.com/job/1',
    ats_platform_name: 'Greenhouse',
    scraped_timestamp: '2026-01-01T00:00:00.000Z',
    location: 'Remote',
    posted_at: '2026-01-01',
  });
});

test('rejects unknown fields, blanks, bad URLs and bad timestamps', () => {
  assert.equal(validateJob({ ...good, extra: 'nope' }), null);
  assert.equal(validateJob({ ...good, title: '   ' }), null);
  assert.equal(
    validateJob({ ...good, direct_apply_url: 'ftp://example.com' }),
    null
  );
  assert.equal(validateJob({ ...good, scraped_timestamp: 'not-a-date' }), null);
  assert.equal(validateJob(null), null);
});

test('validateJobs drops malformed leads from a batch', () => {
  assert.deepEqual(validateJobs([], 'smoke'), []);
  const out = validateJobs([good, { ...good, title: '' }], 'smoke');
  assert.equal(out.length, 1);
  assert.equal(out[0].title, 'Senior Engineer');
});
