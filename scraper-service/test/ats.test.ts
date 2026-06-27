/**
 * Native ATS detection (src/lib/ats.ts). Ported from the former root
 * test/ats-detector.test.js — the same URLs must resolve to the same platform.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';

import { detectAts } from '../src/lib/ats.js';

test('detects built-in jobs that link directly to supported ATSes', () => {
  assert.deepEqual(
    detectAts('https://job-boards.greenhouse.io/halcyon/jobs/5842441004'),
    {
      platform: 'Greenhouse',
      company: 'halcyon',
    }
  );
  assert.deepEqual(
    detectAts(
      'https://jobs.ashbyhq.com/Solvd/00fdc6ea-b992-4772-b999-d4dca8efbdc1/application'
    ),
    { platform: 'Ashby', company: 'Solvd' }
  );
  assert.deepEqual(
    detectAts(
      'https://gsk.wd5.myworkdayjobs.com/GSKCareers/job/Seattle-Sixth-Ave/Senior-AI-ML-Platform-Engineer_431250'
    ),
    { platform: 'Workday', company: 'gsk' }
  );
  assert.deepEqual(detectAts('https://jobs.lever.co/acme/123'), {
    platform: 'Lever',
    company: 'acme',
  });
});

test('detects custom-domain greenhouse links by gh_jid', () => {
  assert.deepEqual(
    detectAts('https://example.applytojob.com/apply/foo?gh_jid=123456'),
    {
      platform: 'Greenhouse',
      company: null,
    }
  );
});

test('returns null for non-ATS and empty input', () => {
  assert.equal(detectAts(''), null);
  assert.equal(detectAts('https://example.com/careers'), null);
});
