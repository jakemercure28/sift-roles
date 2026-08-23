/**
 * Per-host 429 circuit breaker.
 *
 * Without it the Workday path fans out DETAIL_CONCURRENCY parallel detail
 * fetches per company and, against a throttling tenant, every one came back 429
 * while the loop immediately fired the next: one prod pass logged 3,896 Workday
 * 429s. The breaker parks a host after its first 429 so the rest of that board
 * costs nothing.
 */
import { test } from 'node:test';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import assert from 'node:assert/strict';

import { HostBreaker } from '../src/lib/http.js';

const A = 'https://stryker.wd1.myworkdayjobs.com/wday/cxs/a/b/jobs';
const B = 'https://pvh.wd5.myworkdayjobs.com/wday/cxs/a/b/jobs';

test('a fresh breaker blocks nothing', () => {
  assert.equal(new HostBreaker().blocked(A), false);
});

test('a 429 parks that host and only that host', () => {
  const breaker = new HostBreaker();
  breaker.trip(A);
  assert.equal(breaker.blocked(A), true);
  // Each Workday tenant is its own host; one throttling must not stall the rest.
  assert.equal(breaker.blocked(B), false);
});

test('the cooldown expires', () => {
  const breaker = new HostBreaker();
  const t0 = 1_000_000;
  breaker.trip(A, null, t0);
  assert.equal(breaker.blocked(A, t0 + 59_000), true);
  assert.equal(breaker.blocked(A, t0 + 61_000), false);
});

test('repeat 429s back off exponentially', () => {
  const breaker = new HostBreaker();
  const t0 = 1_000_000;
  breaker.trip(A, null, t0);
  breaker.trip(A, null, t0); // second strike doubles to 120s
  assert.equal(breaker.blocked(A, t0 + 61_000), true);
  assert.equal(breaker.blocked(A, t0 + 121_000), false);
});

test('a longer Retry-After in seconds wins over our backoff', () => {
  const breaker = new HostBreaker();
  const t0 = 1_000_000;
  breaker.trip(A, '300', t0);
  assert.equal(breaker.blocked(A, t0 + 299_000), true);
  assert.equal(breaker.blocked(A, t0 + 301_000), false);
});

test('a Retry-After shorter than our backoff does not shorten the park', () => {
  const breaker = new HostBreaker();
  const t0 = 1_000_000;
  breaker.trip(A, '1', t0);
  assert.equal(breaker.blocked(A, t0 + 30_000), true);
});

test('cooldown is capped so a hostile Retry-After cannot park a host forever', () => {
  const breaker = new HostBreaker();
  const t0 = 1_000_000;
  breaker.trip(A, '86400', t0);
  assert.equal(breaker.blocked(A, t0 + 15 * 60_000 + 1_000), false);
});

test('an HTTP-date Retry-After is honored', () => {
  const breaker = new HostBreaker();
  const t0 = Date.parse('2026-07-29T12:00:00Z');
  breaker.trip(A, new Date(t0 + 120_000).toUTCString(), t0);
  assert.equal(breaker.blocked(A, t0 + 60_000), true);
  assert.equal(breaker.blocked(A, t0 + 121_000), false);
});

test('a clean response clears the park and the strike count', () => {
  const breaker = new HostBreaker();
  const t0 = 1_000_000;
  breaker.trip(A, null, t0);
  breaker.clear(A);
  assert.equal(breaker.blocked(A, t0), false);
  // Strikes reset too, so the next 429 starts from the base cooldown again.
  breaker.trip(A, null, t0);
  assert.equal(breaker.blocked(A, t0 + 61_000), false);
});

test('an unparseable url is never treated as blocked', () => {
  const breaker = new HostBreaker();
  breaker.trip('not a url');
  assert.equal(breaker.blocked('not a url'), false);
});

test('safeFetch trips the breaker on 429 and then skips the host entirely', async () => {
  const { safeFetch } = await import('../src/lib/http.js');
  const server = createServer((_req, res) => {
    calls += 1;
    res.writeHead(429, { 'retry-after': '120' });
    res.end('slow down');
  });
  let calls = 0;
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  const { port } = server.address() as AddressInfo;
  const url = `http://127.0.0.1:${port}/jobs`;
  try {
    const breaker = new HostBreaker();
    assert.equal(await safeFetch(url, {}, 'test', breaker), null);
    assert.equal(calls, 1);

    // Second call must short-circuit: no round trip to be refused again.
    assert.equal(await safeFetch(url, {}, 'test', breaker), null);
    assert.equal(calls, 1);
  } finally {
    server.close();
  }
});

test('safeFetch without a breaker is unchanged', async () => {
  const { safeFetch } = await import('../src/lib/http.js');
  let calls = 0;
  const server = createServer((_req, res) => {
    calls += 1;
    res.writeHead(429);
    res.end('slow down');
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  const { port } = server.address() as AddressInfo;
  try {
    const url = `http://127.0.0.1:${port}/jobs`;
    assert.equal(await safeFetch(url, {}, 'test'), null);
    assert.equal(await safeFetch(url, {}, 'test'), null);
    assert.equal(calls, 2);
  } finally {
    server.close();
  }
});
