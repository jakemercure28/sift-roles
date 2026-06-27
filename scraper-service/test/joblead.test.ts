/**
 * Golden snapshot for the makeJobLead constructor and deriveJobId. These used to
 * diff against lib/job-lead.js (the CommonJS original); that file was retired once
 * the TS port was confirmed byte-identical, so the committed snapshots in
 * fixtures/joblead/ are now the source of truth. Regenerate with UPDATE_GOLDEN=1.
 */
import { test } from 'node:test';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

import { assertJsonGolden } from './parity/harness.js';

const here = dirname(fileURLToPath(import.meta.url));
const fixtures = resolve(here, 'fixtures', 'joblead');

const INPUTS = [
  {
    title: '  Senior Engineer  ',
    company: 'Acme',
    description: ' hello ',
    directApplyUrl: 'https://example.com/job/1 ',
    atsPlatformName: 'Greenhouse',
    scrapedTimestamp: '2026-01-01T00:00:00.000Z',
    location: '  Remote  ',
    postedAt: ' 2026-01-01 ',
  },
  {
    // missing optionals -> defaults; missing required -> trimmed empties
    title: 'Only Title',
    directApplyUrl: 'https://example.com/job/2',
    atsPlatformName: 'Greenhouse',
    scrapedTimestamp: '2026-01-01T00:00:00.000Z',
  },
  {
    title: undefined,
    company: undefined,
    description: undefined,
    directApplyUrl: undefined,
    atsPlatformName: undefined,
    scrapedTimestamp: '2026-01-01T00:00:00.000Z',
  },
];

test('makeJobLead matches committed golden', async () => {
  const { makeJobLead } = await import('../src/lib/joblead.js');
  const out = INPUTS.map((input) => makeJobLead(input));
  assertJsonGolden(out, resolve(fixtures, 'make.json'));
});

// One per branch of deriveJobId: each ATS platform's URL parser plus the
// non-ATS sha256 fallback (and the empty-url edge), covering the hostname,
// path-index, and slug paths.
const ID_INPUTS = [
  // greenhouse: /jobs/<id> anywhere in the path
  {
    direct_apply_url: 'https://boards.greenhouse.io/acme/jobs/12345',
    ats_platform_name: 'Greenhouse',
  },
  // ashby: jobs.ashbyhq.com/<board>/<uuid>
  {
    direct_apply_url:
      'https://jobs.ashbyhq.com/acme/6b2ee1c2-509e-4433-9a60-3f79d7dfcd42',
    ats_platform_name: 'Ashby',
  },
  // lever: jobs.lever.co/<company>/<uuid>
  {
    direct_apply_url:
      'https://jobs.lever.co/acme/0c9f1d2e-1111-2222-3333-444455556666',
    ats_platform_name: 'Lever',
  },
  // workday: *.myworkdayjobs.com, slug(host) + slug(last path segment)
  {
    direct_apply_url:
      'https://acme.wd5.myworkdayjobs.com/en-US/External/job/Senior-Engineer_R-123',
    ats_platform_name: 'Workday',
  },
  // non-ATS host -> platform slug + sha256(platform|url) fallback
  {
    direct_apply_url: 'https://careers.example.com/postings/abc',
    ats_platform_name: 'Custom ATS',
  },
  // legacy `url`/`platform` field names (not direct_apply_url/ats_platform_name)
  { url: 'https://boards.greenhouse.io/acme/jobs/999', platform: 'Greenhouse' },
  // empty/garbage url exercises the catch + hash path
  { direct_apply_url: '', ats_platform_name: '' },
  { direct_apply_url: 'not a url', ats_platform_name: 'Whatever' },
];

test('deriveJobId matches committed golden', async () => {
  const { deriveJobId } = await import('../src/lib/joblead.js');
  const out = ID_INPUTS.map((input) => deriveJobId(input));
  assertJsonGolden(out, resolve(fixtures, 'ids.json'));
});
