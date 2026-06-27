/**
 * Native onboarding gate (src/lib/onboarding.ts). Ported from the former root
 * test/onboarding-gate.test.js: marker wins, fresh installs (demo companies
 * copied verbatim) stay gated, and a real-looking pre-marker install auto-heals.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  existsSync,
  rmSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import {
  isOnboarded,
  markOnboarded,
  markerPath,
  MARKER_NAME,
} from '../src/lib/onboarding.js';

const DEMO_COMPANIES = JSON.stringify(
  { SEARCH_TERMS: [], GREENHOUSE_COMPANIES: ['stripe', 'datadog'] },
  null,
  2
);

function makeFixture(): { root: string; profileDir: string } {
  const root = mkdtempSync(join(tmpdir(), 'jsa-onboard-'));
  const profileDir = join(root, 'data');
  mkdirSync(profileDir, { recursive: true });
  mkdirSync(join(root, 'data.example'), { recursive: true });
  writeFileSync(
    join(root, 'data.example', 'companies.json'),
    DEMO_COMPANIES,
    'utf8'
  );
  return { root, profileDir };
}

test('returns false on a fresh install (demo companies copied verbatim, no marker)', () => {
  const fx = makeFixture();
  try {
    writeFileSync(
      join(fx.profileDir, 'companies.json'),
      DEMO_COMPANIES,
      'utf8'
    );
    writeFileSync(join(fx.profileDir, 'resume.md'), 'a resume', 'utf8');
    assert.equal(
      isOnboarded({
        profileDir: fx.profileDir,
        repoRoot: fx.root,
        env: { GEMINI_API_KEY: 'key' },
      }),
      false
    );
  } finally {
    rmSync(fx.root, { recursive: true, force: true });
  }
});

test('returns true when the marker exists, regardless of profile contents', () => {
  const fx = makeFixture();
  try {
    markOnboarded(fx.profileDir);
    assert.equal(
      isOnboarded({ profileDir: fx.profileDir, repoRoot: fx.root, env: {} }),
      true
    );
  } finally {
    rmSync(fx.root, { recursive: true, force: true });
  }
});

test('auto-heals a pre-marker install with key + resume + customized companies', () => {
  const fx = makeFixture();
  try {
    writeFileSync(join(fx.profileDir, 'resume.md'), 'real resume', 'utf8');
    writeFileSync(
      join(fx.profileDir, 'companies.json'),
      JSON.stringify({ SEARCH_TERMS: ['devops'], GREENHOUSE_COMPANIES: [] }),
      'utf8'
    );
    assert.equal(existsSync(markerPath(fx.profileDir)), false);
    assert.equal(
      isOnboarded({
        profileDir: fx.profileDir,
        repoRoot: fx.root,
        env: { GEMINI_API_KEY: 'key' },
      }),
      true
    );
    assert.equal(existsSync(markerPath(fx.profileDir)), true);
  } finally {
    rmSync(fx.root, { recursive: true, force: true });
  }
});

test('auto-heals when companies.json matches the example but discovery found boards', () => {
  // Natalie's case: the tenant set no manual companies/search terms, so
  // companies.json stayed identical to the example, but resume-driven discovery
  // appended verified boards to suggested-companies.json (which the scraper
  // merges in and scrapes). The gate must recognize that as scrapeable.
  const fx = makeFixture();
  try {
    writeFileSync(join(fx.profileDir, 'resume.md'), 'real resume', 'utf8');
    writeFileSync(
      join(fx.profileDir, 'companies.json'),
      DEMO_COMPANIES,
      'utf8'
    );
    writeFileSync(
      join(fx.profileDir, 'suggested-companies.json'),
      JSON.stringify({
        greenhouse: ['airbnb', 'stripe'],
        ashby: [],
        lever: [],
        workday: [],
      }),
      'utf8'
    );
    assert.equal(existsSync(markerPath(fx.profileDir)), false);
    assert.equal(
      isOnboarded({
        profileDir: fx.profileDir,
        repoRoot: fx.root,
        env: { GEMINI_API_KEY: 'key' },
      }),
      true
    );
    assert.equal(existsSync(markerPath(fx.profileDir)), true);
  } finally {
    rmSync(fx.root, { recursive: true, force: true });
  }
});

test('stays gated when companies match the example and discovery found nothing', () => {
  const fx = makeFixture();
  try {
    writeFileSync(join(fx.profileDir, 'resume.md'), 'real resume', 'utf8');
    writeFileSync(
      join(fx.profileDir, 'companies.json'),
      DEMO_COMPANIES,
      'utf8'
    );
    writeFileSync(
      join(fx.profileDir, 'suggested-companies.json'),
      JSON.stringify({ greenhouse: [], ashby: [], lever: [], workday: [] }),
      'utf8'
    );
    assert.equal(
      isOnboarded({
        profileDir: fx.profileDir,
        repoRoot: fx.root,
        env: { GEMINI_API_KEY: 'key' },
      }),
      false
    );
  } finally {
    rmSync(fx.root, { recursive: true, force: true });
  }
});

test('does not auto-heal when the key is missing', () => {
  const fx = makeFixture();
  try {
    writeFileSync(join(fx.profileDir, 'resume.md'), 'real resume', 'utf8');
    writeFileSync(join(fx.profileDir, 'companies.json'), '{}\n', 'utf8');
    assert.equal(
      isOnboarded({ profileDir: fx.profileDir, repoRoot: fx.root, env: {} }),
      false
    );
  } finally {
    rmSync(fx.root, { recursive: true, force: true });
  }
});

test('exposes a stable marker name', () => {
  const fx = makeFixture();
  try {
    assert.equal(MARKER_NAME, '.onboarded');
    assert.equal(markerPath(fx.profileDir), join(fx.profileDir, '.onboarded'));
  } finally {
    rmSync(fx.root, { recursive: true, force: true });
  }
});
