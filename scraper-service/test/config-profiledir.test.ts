/**
 * Per-request profile-dir override (src/lib/interop.ts + src/lib/config.ts).
 *
 * Hosted Postgres relocates each profile into a per-tenant dir and the Go
 * backend passes that dir in the /scrape payload. setProfileDir must repoint the
 * worker's reads, and companyConfig() must re-read when the active dir changes
 * (its cache is keyed by dir) rather than pinning the first tenant's config.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { setProfileDir, getProfileDir, DATA_DIR } from '../src/lib/interop.js';
import { companyConfig } from '../src/lib/config.js';
import { deriveSearchTermsFromResumeText } from '../src/lib/resume_terms.js';

function makeProfile(term: string, slug: string): string {
  const dir = mkdtempSync(join(tmpdir(), 'jsa-profile-'));
  mkdirSync(dir, { recursive: true });
  writeFileSync(
    join(dir, 'companies.json'),
    JSON.stringify({ SEARCH_TERMS: [term], GREENHOUSE_COMPANIES: [slug] }),
    'utf8'
  );
  return dir;
}

test('companyConfig re-reads when the active profile dir changes', () => {
  const a = makeProfile('devops', 'acme');
  const b = makeProfile('platform', 'globex');
  try {
    setProfileDir(a);
    const cfgA = companyConfig();
    assert.deepEqual(cfgA.SEARCH_TERMS, ['devops']);
    assert.deepEqual(cfgA.GREENHOUSE_COMPANIES, ['acme']);

    // Switching tenants must NOT return the cached first-tenant config.
    setProfileDir(b);
    const cfgB = companyConfig();
    assert.deepEqual(cfgB.SEARCH_TERMS, ['platform']);
    assert.deepEqual(cfgB.GREENHOUSE_COMPANIES, ['globex']);
  } finally {
    setProfileDir(undefined);
    rmSync(a, { recursive: true, force: true });
    rmSync(b, { recursive: true, force: true });
  }
});

test('companyConfig re-reads when suggestions are written for the active dir', () => {
  const a = makeProfile('devops', 'acme');
  try {
    setProfileDir(a);
    const before = companyConfig();
    assert.deepEqual(before.GREENHOUSE_COMPANIES, ['acme']);

    writeFileSync(
      join(a, 'suggested-companies.json'),
      JSON.stringify({ greenhouse: ['globex'], ashby: ['ashco'] }),
      'utf8'
    );

    const after = companyConfig();
    assert.deepEqual(after.GREENHOUSE_COMPANIES, ['acme', 'globex']);
    assert.deepEqual(after.ASHBY_COMPANIES, ['ashco']);
  } finally {
    setProfileDir(undefined);
    rmSync(a, { recursive: true, force: true });
  }
});

test('companyConfig derives search terms from resume when companies has none', () => {
  const dir = mkdtempSync(join(tmpdir(), 'jsa-profile-'));
  try {
    mkdirSync(dir, { recursive: true });
    writeFileSync(
      join(dir, 'companies.json'),
      JSON.stringify({ SEARCH_TERMS: [], GREENHOUSE_COMPANIES: ['acme'] }),
      'utf8'
    );
    writeFileSync(
      join(dir, 'resume.md'),
      'Visual Merchandiser - Nordstrom\nRetail merchandising and assortment planning.',
      'utf8'
    );

    setProfileDir(dir);
    const cfg = companyConfig();
    assert.deepEqual(cfg.SEARCH_TERMS.slice(0, 2), [
      'visual merchandiser',
      'merchandising',
    ]);
  } finally {
    setProfileDir(undefined);
    rmSync(dir, { recursive: true, force: true });
  }
});

test('deriveSearchTermsFromResumeText extracts role-like resume lines', () => {
  assert.deepEqual(
    deriveSearchTermsFromResumeText(
      'Senior Data Analyst | Acme\nBuilt dashboards for operations leaders.'
    ).slice(0, 2),
    ['senior data analyst', 'data analyst']
  );
});

test('setProfileDir(undefined) resets to the default DATA_DIR', () => {
  const a = makeProfile('devops', 'acme');
  try {
    setProfileDir(a);
    assert.equal(getProfileDir(), a);
    setProfileDir(undefined);
    assert.equal(getProfileDir(), DATA_DIR);
  } finally {
    setProfileDir(undefined);
    rmSync(a, { recursive: true, force: true });
  }
});
