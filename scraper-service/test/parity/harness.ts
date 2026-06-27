/**
 * Shared helpers for the scraper golden tests: a deterministic fetch mock that
 * serves committed fixtures, plus golden-file comparators that assert a native
 * TS scraper's output against a committed expected.json.
 *
 * These used to be "parity" tests that diffed the TS scraper against a parallel
 * CommonJS scraper in <repo>/scrapers/*.js. Those JS scrapers (and lib/job-lead.js)
 * were retired once every scraper was confirmed byte-identical; the committed
 * expected.json snapshots are now the source of truth. Regenerate them after an
 * intentional mapping change with UPDATE_GOLDEN=1 (see scripts in package.json).
 */
import assert from 'node:assert/strict';
import { readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import { dirname } from 'node:path';

type Lead = {
  direct_apply_url: string;
  scraped_timestamp: string;
  [key: string]: unknown;
};

function urlOf(input: unknown): string {
  if (typeof input === 'string') return input;
  if (input instanceof URL) return input.href;
  const req = input as { url?: string };
  return typeof req?.url === 'string' ? req.url : String(input);
}

/**
 * Replace global fetch with one that serves `resolve(url)` as a 200 JSON body
 * (or 404 when it returns undefined). Returns a restore function.
 */
export function installFetchMock(
  resolve: (url: string) => unknown
): () => void {
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: unknown) => {
    const body = resolve(urlOf(input));
    if (body === undefined) return new Response('Not found', { status: 404 });
    // String bodies (e.g. RSS/XML feeds) pass through as raw text; objects are
    // serialized as JSON so res.json() parses them.
    const isText = typeof body === 'string';
    return new Response(isText ? (body as string) : JSON.stringify(body), {
      status: 200,
      headers: {
        'content-type': isText ? 'text/plain' : 'application/json',
      },
    });
  }) as typeof fetch;
  return () => {
    globalThis.fetch = original;
  };
}

/**
 * Normalize leads to the stable shape that gets committed: zero out the only
 * field that legitimately varies run-to-run (scraped_timestamp, which is
 * `new Date().toISOString()`), and sort by apply URL so order is deterministic.
 */
export function normalizeLeads(leads: Lead[]): Lead[] {
  return [...leads]
    .map((lead) => ({ ...lead, scraped_timestamp: '' }))
    .sort((a, b) =>
      String(a.direct_apply_url).localeCompare(String(b.direct_apply_url))
    );
}

const UPDATE = process.env.UPDATE_GOLDEN === '1';

function writeGolden(goldenPath: string, value: unknown): void {
  mkdirSync(dirname(goldenPath), { recursive: true });
  writeFileSync(goldenPath, JSON.stringify(value, null, 2) + '\n');
}

/**
 * Assert that a scraper's leads match the committed golden snapshot. With
 * UPDATE_GOLDEN=1 the snapshot is (re)written instead of compared.
 */
export function assertGolden(
  tsLeads: Lead[],
  goldenPath: string,
  opts: { minCount?: number } = {}
): void {
  const normalized = normalizeLeads(tsLeads);
  if (UPDATE) {
    writeGolden(goldenPath, normalized);
    return;
  }
  const expected = JSON.parse(readFileSync(goldenPath, 'utf8'));
  assert.deepEqual(normalized, expected);
  if (opts.minCount != null) {
    assert.ok(
      normalized.length >= opts.minCount,
      `expected >= ${opts.minCount} leads, got ${normalized.length}`
    );
  }
}

/**
 * Generic golden comparator for non-lead values (e.g. joblead snapshots). No
 * normalization. With UPDATE_GOLDEN=1 the snapshot is (re)written.
 */
export function assertJsonGolden(value: unknown, goldenPath: string): void {
  if (UPDATE) {
    writeGolden(goldenPath, value);
    return;
  }
  const expected = JSON.parse(readFileSync(goldenPath, 'utf8'));
  assert.deepEqual(value, expected);
}
