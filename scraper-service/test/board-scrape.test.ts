/**
 * Board mode: runBoardScrape must fetch exactly the given boards, keep EVERY
 * posting (no search-term filtering), read no profile config, and derive the
 * same job ids as the per-tenant path.
 */
import assert from 'node:assert/strict';
import { test } from 'node:test';

import { installFetchMock } from './parity/harness.js';
import { runBoardScrape } from '../src/bridge.js';

test('board mode scrapes explicit greenhouse/ashby boards without term filtering', async () => {
  const restore = installFetchMock((url) => {
    if (url.includes('boards-api.greenhouse.io/v1/boards/acme/jobs')) {
      return {
        jobs: [
          {
            title: 'Underwater Basket Weaver', // matches no search terms anywhere
            absolute_url: 'https://boards.greenhouse.io/acme/jobs/111',
            content: 'Weave baskets.',
            location: { name: 'Remote' },
          },
          {
            title: 'Chief Vibes Officer',
            absolute_url: 'https://boards.greenhouse.io/acme/jobs/222',
            content: 'Vibes.',
            location: { name: 'NYC' },
          },
        ],
      };
    }
    if (url.includes('api.ashbyhq.com/posting-api/job-board/globex')) {
      return {
        jobs: [
          {
            title: 'Snail Trainer',
            descriptionPlain: 'Train snails.',
            jobUrl: 'https://jobs.ashbyhq.com/globex/abc-123',
            location: 'Remote',
          },
        ],
      };
    }
    return undefined;
  });
  try {
    const { jobs } = await runBoardScrape([
      { platform: 'greenhouse', slug: 'acme' },
      { platform: 'ashby', slug: 'globex', label: 'Globex' },
    ]);
    const ids = jobs.map((j) => j.id).sort();
    assert.deepEqual(ids, [
      'ashby-abc-123',
      'greenhouse-111',
      'greenhouse-222',
    ]);
    // No term filter ran: these titles match no plausible search-term list.
    assert.equal(jobs.length, 3);
  } finally {
    restore();
  }
});

test('workday board mode paginates the full board and keeps every posting', async () => {
  const TOTAL = 25;
  const postings = Array.from({ length: TOTAL }, (_, i) => ({
    title: `Zzz Role ${i}`,
    externalPath: `/job/somewhere/zzz-role-${i}_JR${1000 + i}`,
    locationsText: 'Seattle, WA',
  }));

  const listOffsets: number[] = [];
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: unknown, init?: RequestInit) => {
    const url =
      typeof input === 'string' ? input : (input as { url: string }).url;
    if (url.endsWith('/wday/cxs/megacorp/External/jobs')) {
      const body = JSON.parse(String(init?.body ?? '{}')) as {
        offset?: number;
        limit?: number;
        searchText?: string;
      };
      assert.equal(body.searchText, '', 'board mode must not search by term');
      const offset = body.offset ?? 0;
      listOffsets.push(offset);
      return new Response(
        JSON.stringify({
          total: TOTAL,
          jobPostings: postings.slice(offset, offset + (body.limit ?? 20)),
        }),
        { status: 200, headers: { 'content-type': 'application/json' } }
      );
    }
    if (url.includes('/wday/cxs/megacorp/External/job/')) {
      return new Response(
        JSON.stringify({
          jobPostingInfo: {
            jobDescription: '<p>Do the thing.</p>',
            location: 'Seattle, WA',
          },
        }),
        { status: 200, headers: { 'content-type': 'application/json' } }
      );
    }
    return new Response('Not found', { status: 404 });
  }) as typeof fetch;

  try {
    const { jobs } = await runBoardScrape([
      {
        platform: 'workday',
        slug: 'megacorp',
        wd: 5,
        board: 'External',
        label: 'MegaCorp',
      },
    ]);
    assert.equal(jobs.length, TOTAL);
    assert.deepEqual(listOffsets, [0, 20], 'paginates by 20 until total');
    assert.equal(jobs[0].company, 'MegaCorp');
    assert.ok(jobs[0].id.startsWith('workday-'));
    assert.ok(
      jobs[0].direct_apply_url.startsWith(
        'https://megacorp.wd5.myworkdayjobs.com/en-US/External/job/'
      )
    );
  } finally {
    globalThis.fetch = original;
  }
});

test('board mode dedupes repeated boards and cross-board duplicate leads', async () => {
  let boardFetches = 0;
  const restore = installFetchMock((url) => {
    if (url.includes('boards-api.greenhouse.io/v1/boards/acme/jobs')) {
      boardFetches++;
      return {
        jobs: [
          {
            title: 'Solo Role',
            absolute_url: 'https://boards.greenhouse.io/acme/jobs/999',
            content: 'One.',
          },
        ],
      };
    }
    return undefined;
  });
  try {
    const { jobs } = await runBoardScrape([
      { platform: 'greenhouse', slug: 'acme' },
      { platform: 'greenhouse', slug: 'acme' }, // duplicate slug collapses
    ]);
    assert.equal(boardFetches, 1);
    assert.equal(jobs.length, 1);
    assert.equal(jobs[0].id, 'greenhouse-999');
  } finally {
    restore();
  }
});

test('board mode skips unchanged boards via ETag and content hash', async () => {
  const body = JSON.stringify({
    jobs: [
      {
        title: 'Solo Role',
        absolute_url: 'https://boards.greenhouse.io/acme/jobs/999',
        content: 'One.',
      },
    ],
  });
  let fetches = 0;
  let sawIfNoneMatch: string | null = null;
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: unknown, init?: RequestInit) => {
    const url =
      typeof input === 'string' ? input : (input as { url: string }).url;
    if (!url.includes('boards-api.greenhouse.io')) {
      return new Response('Not found', { status: 404 });
    }
    fetches++;
    const headers = new Headers(init?.headers as HeadersInit);
    sawIfNoneMatch = headers.get('if-none-match');
    if (sawIfNoneMatch === 'W/"v1"') {
      return new Response(null, { status: 304 });
    }
    return new Response(body, {
      status: 200,
      headers: { 'content-type': 'application/json', etag: 'W/"v1"' },
    });
  }) as typeof fetch;

  try {
    // First cycle: no state, full fetch, outcome carries etag + hash.
    const first = await runBoardScrape([
      { platform: 'greenhouse', slug: 'acme' },
    ]);
    assert.equal(first.jobs.length, 1);
    assert.equal(first.boards.length, 1);
    assert.equal(first.boards[0].status, 'ok');
    assert.equal(first.boards[0].etag, 'W/"v1"');
    assert.ok(first.boards[0].contentHash);

    // Second cycle with the stored etag: the server 304s, no jobs re-parsed.
    const second = await runBoardScrape([
      {
        platform: 'greenhouse',
        slug: 'acme',
        etag: first.boards[0].etag,
        contentHash: first.boards[0].contentHash,
      },
    ]);
    assert.equal(sawIfNoneMatch, 'W/"v1"');
    assert.equal(second.jobs.length, 0);
    assert.equal(second.boards[0].status, 'unchanged');
    assert.equal(second.boards[0].etag, 'W/"v1"');
    assert.equal(second.boards[0].contentHash, first.boards[0].contentHash);

    // Third cycle with only a content hash (no etag): body hash still skips.
    const third = await runBoardScrape([
      {
        platform: 'greenhouse',
        slug: 'acme',
        contentHash: first.boards[0].contentHash,
      },
    ]);
    assert.equal(third.jobs.length, 0);
    assert.equal(third.boards[0].status, 'unchanged');
    assert.equal(fetches, 3);
  } finally {
    globalThis.fetch = original;
  }
});

test('workday board reports ok when the listing completed', async () => {
  const TOTAL = 3;
  const postings = Array.from({ length: TOTAL }, (_, i) => ({
    title: `Role ${i}`,
    externalPath: `/job/somewhere/role-${i}_JR${2000 + i}`,
    locationsText: 'Seattle, WA',
  }));
  const restore = installFetchMock((url) => {
    if (url.endsWith('/wday/cxs/megacorp/External/jobs')) {
      return { total: TOTAL, jobPostings: postings };
    }
    if (url.includes('/wday/cxs/megacorp/External/job/')) {
      return { jobPostingInfo: { jobDescription: 'x', location: 'Seattle' } };
    }
    return undefined;
  });
  try {
    const { jobs, boards } = await runBoardScrape([
      {
        platform: 'workday',
        slug: 'megacorp',
        wd: 5,
        board: 'External',
        label: 'MegaCorp',
      },
    ]);
    assert.equal(jobs.length, TOTAL);
    assert.deepEqual(boards, [
      {
        platform: 'workday',
        slug: 'megacorp',
        company: 'MegaCorp',
        status: 'ok',
      },
    ]);
  } finally {
    restore();
  }
});

test('workday board blocked mid-pagination reports failed, not end-of-board', async () => {
  const TOTAL = 25; // two pages; the second one is WAF-blocked
  const postings = Array.from({ length: TOTAL }, (_, i) => ({
    title: `Role ${i}`,
    externalPath: `/job/somewhere/role-${i}_JR${3000 + i}`,
    locationsText: 'Seattle, WA',
  }));
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: unknown, init?: RequestInit) => {
    const url =
      typeof input === 'string' ? input : (input as { url: string }).url;
    if (url.endsWith('/wday/cxs/megacorp/External/jobs')) {
      const body = JSON.parse(String(init?.body ?? '{}')) as {
        offset?: number;
      };
      const offset = body.offset ?? 0;
      if (offset >= 20) return new Response('blocked', { status: 403 });
      return new Response(
        JSON.stringify({ total: TOTAL, jobPostings: postings.slice(0, 20) }),
        { status: 200, headers: { 'content-type': 'application/json' } }
      );
    }
    if (url.includes('/wday/cxs/megacorp/External/job/')) {
      return new Response(
        JSON.stringify({ jobPostingInfo: { jobDescription: 'x' } }),
        { status: 200, headers: { 'content-type': 'application/json' } }
      );
    }
    return new Response('Not found', { status: 404 });
  }) as typeof fetch;
  try {
    const { jobs, boards } = await runBoardScrape([
      {
        platform: 'workday',
        slug: 'megacorp',
        wd: 5,
        board: 'External',
        label: 'MegaCorp',
      },
    ]);
    // The postings that did come back are still harvested; the board is just
    // barred from the closed diff.
    assert.equal(jobs.length, 20);
    assert.equal(boards?.[0]?.status, 'failed');
    assert.equal(boards?.[0]?.company, 'MegaCorp');
  } finally {
    globalThis.fetch = original;
  }
});

test('workday board larger than the pagination cap reports failed', async () => {
  const TOTAL = 1100; // over BOARD_MAX_JOBS: the listing is a truncated view
  const restore = installFetchMock((url) => {
    if (url.endsWith('/wday/cxs/megacorp/External/jobs')) {
      // Every page is full; parse of the POST body is not needed because the
      // content is identical page to page (ids collide, which also exercises
      // the seen-set) — the cap decision only depends on total.
      return {
        total: TOTAL,
        jobPostings: Array.from({ length: 20 }, (_, i) => ({
          title: `Role ${i}`,
          externalPath: `/job/somewhere/role-${i}_JR${4000 + i}`,
          locationsText: 'Seattle, WA',
        })),
      };
    }
    if (url.includes('/wday/cxs/megacorp/External/job/')) {
      return { jobPostingInfo: { jobDescription: 'x' } };
    }
    return undefined;
  });
  try {
    const { boards } = await runBoardScrape([
      {
        platform: 'workday',
        slug: 'megacorp',
        wd: 5,
        board: 'External',
        label: 'MegaCorp',
      },
    ]);
    assert.equal(boards?.[0]?.status, 'failed');
  } finally {
    restore();
  }
});

test('workday garbage total:0 mid-pagination must not read as end-of-board', async () => {
  // Observed on prod (Accenture/Autodesk 2026-07-04): the cxs endpoint reports
  // total: 0 on some pages while still serving full pages of postings. Reading
  // offset+page >= total as completion truncated the listing and closed live
  // jobs. Such a board must report failed (it exceeds the pagination cap).
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: unknown, init?: RequestInit) => {
    const url =
      typeof input === 'string' ? input : (input as { url: string }).url;
    if (url.endsWith('/wday/cxs/megacorp/External/jobs')) {
      const body = JSON.parse(String(init?.body ?? '{}')) as {
        offset?: number;
      };
      const offset = body.offset ?? 0;
      return new Response(
        JSON.stringify({
          // First page reports the (server-capped) total; later pages report 0.
          total: offset === 0 ? 2000 : 0,
          jobPostings: Array.from({ length: 20 }, (_, i) => ({
            title: `Role ${offset + i}`,
            externalPath: `/job/somewhere/role-${offset + i}_JR${5000 + offset + i}`,
            locationsText: 'Seattle, WA',
          })),
        }),
        { status: 200, headers: { 'content-type': 'application/json' } }
      );
    }
    if (url.includes('/wday/cxs/megacorp/External/job/')) {
      return new Response(
        JSON.stringify({ jobPostingInfo: { jobDescription: 'x' } }),
        { status: 200, headers: { 'content-type': 'application/json' } }
      );
    }
    return new Response('Not found', { status: 404 });
  }) as typeof fetch;
  try {
    const { boards } = await runBoardScrape([
      {
        platform: 'workday',
        slug: 'megacorp',
        wd: 5,
        board: 'External',
        label: 'MegaCorp',
      },
    ]);
    assert.equal(boards?.[0]?.status, 'failed');
  } finally {
    globalThis.fetch = original;
  }
});

test('workday empty page with total:0 mid-pagination must not read as end-of-board', async () => {
  // Observed on prod (autodesk/visa/intel 2026-07-04): mid-pagination the WAF
  // serves { jobPostings: [], total: 0 }. Treating total: 0 as "unknown" and
  // the empty page as a clean end reported a truncated listing as complete,
  // and the closed diff buried every live posting past the truncation point.
  // A short page without a positive corroborating total must report failed.
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: unknown, init?: RequestInit) => {
    const url =
      typeof input === 'string' ? input : (input as { url: string }).url;
    if (url.endsWith('/wday/cxs/megacorp/External/jobs')) {
      const body = JSON.parse(String(init?.body ?? '{}')) as {
        offset?: number;
      };
      const offset = body.offset ?? 0;
      if (offset >= 40) {
        // WAF garbage: an empty page claiming an empty board.
        return new Response(JSON.stringify({ total: 0, jobPostings: [] }), {
          status: 200,
          headers: { 'content-type': 'application/json' },
        });
      }
      return new Response(
        JSON.stringify({
          total: 2000,
          jobPostings: Array.from({ length: 20 }, (_, i) => ({
            title: `Role ${offset + i}`,
            externalPath: `/job/somewhere/role-${offset + i}_JR${5000 + offset + i}`,
            locationsText: 'Seattle, WA',
          })),
        }),
        { status: 200, headers: { 'content-type': 'application/json' } }
      );
    }
    if (url.includes('/wday/cxs/megacorp/External/job/')) {
      return new Response(
        JSON.stringify({ jobPostingInfo: { jobDescription: 'x' } }),
        { status: 200, headers: { 'content-type': 'application/json' } }
      );
    }
    return new Response('Not found', { status: 404 });
  }) as typeof fetch;
  try {
    const { boards } = await runBoardScrape([
      {
        platform: 'workday',
        slug: 'megacorp',
        wd: 5,
        board: 'External',
        label: 'MegaCorp',
      },
    ]);
    assert.equal(boards?.[0]?.status, 'failed');
  } finally {
    globalThis.fetch = original;
  }
});

test('workday board empty from the first page still reports ok', async () => {
  // A parked/empty board yields zero leads, which never feed the closed diff,
  // so it must not log as a failed fetch on every cycle.
  const original = globalThis.fetch;
  globalThis.fetch = (async (input: unknown) => {
    const url =
      typeof input === 'string' ? input : (input as { url: string }).url;
    if (url.endsWith('/wday/cxs/parked/External/jobs')) {
      return new Response(JSON.stringify({ total: 0, jobPostings: [] }), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      });
    }
    return new Response('Not found', { status: 404 });
  }) as typeof fetch;
  try {
    const { boards } = await runBoardScrape([
      {
        platform: 'workday',
        slug: 'parked',
        wd: 5,
        board: 'External',
        label: 'Parked',
      },
    ]);
    assert.equal(boards?.[0]?.status, 'ok');
  } finally {
    globalThis.fetch = original;
  }
});
