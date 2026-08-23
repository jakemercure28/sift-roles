/**
 * Work-health behavior for the scraper-service /health and /livez endpoints.
 *
 * The point of ScrapeLiveness is that a scrape stuck past the threshold flips
 * /health to 503 so the condition is visible, while a normal scrape that settles
 * leaves health green. /livez is the separate shallow endpoint the kubelet
 * probes, and it must stay 200 through all of that: restarting the container
 * does not un-stick a slow board pass, it only throws the pass away. These tests
 * inject a tracker with a tiny threshold and drive buildServer via Fastify's
 * inject, so nothing waits on real minutes or touches the network.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';

import Fastify from 'fastify';
import type { AddressInfo } from 'node:net';

import { buildServer, clientGoneSignal } from '../src/index.js';
import { ScrapeLiveness } from '../src/health.js';

interface HealthBody {
  status: string;
  scrapeInFlight: number;
  oldestScrapeMs: number | null;
  lastSuccessAt: number | null;
}

test('idle server reports healthy', async () => {
  const app = buildServer({ liveness: new ScrapeLiveness(50) });
  try {
    const res = await app.inject({ method: 'GET', url: '/health' });
    assert.equal(res.statusCode, 200);
    const body = res.json() as HealthBody;
    assert.equal(body.status, 'ok');
    assert.equal(body.scrapeInFlight, 0);
    assert.equal(body.oldestScrapeMs, null);
  } finally {
    await app.close();
  }
});

test('a scrape stuck past the threshold flips /health to 503', async () => {
  const liveness = new ScrapeLiveness(20); // 20ms stuck threshold
  const app = buildServer({ liveness });
  // A scrape that never settles: its start time ages past the threshold.
  let release: () => void = () => {};
  const hung = liveness.track(() => new Promise<void>((r) => (release = r)));
  try {
    await new Promise((r) => setTimeout(r, 40)); // let it overrun

    const res = await app.inject({ method: 'GET', url: '/health' });
    assert.equal(res.statusCode, 503);
    const body = res.json() as HealthBody;
    assert.equal(body.status, 'degraded');
    assert.ok(body.scrapeInFlight >= 1);
    assert.ok((body.oldestScrapeMs ?? 0) > 20);
  } finally {
    release();
    await hung;
    await app.close();
  }
});

test('health returns to ok once a tracked scrape settles', async () => {
  const liveness = new ScrapeLiveness(20);
  const app = buildServer({ liveness });
  try {
    await liveness.track(async () => {
      await new Promise((r) => setTimeout(r, 30)); // overruns while running
    });
    // Settled: in-flight cleared, lastSuccessAt recorded, health green again.
    const res = await app.inject({ method: 'GET', url: '/health' });
    assert.equal(res.statusCode, 200);
    const body = res.json() as HealthBody;
    assert.equal(body.status, 'ok');
    assert.equal(body.scrapeInFlight, 0);
    assert.ok(typeof body.lastSuccessAt === 'number');
  } finally {
    await app.close();
  }
});

test('/livez stays 200 while a scrape is stuck and /health is 503', async () => {
  const liveness = new ScrapeLiveness(20);
  const app = buildServer({ liveness });
  let release: () => void = () => {};
  const hung = liveness.track(() => new Promise<void>((r) => (release = r)));
  try {
    await new Promise((r) => setTimeout(r, 40)); // let it overrun

    // The exact state that was getting the pod SIGKILLed mid board pass.
    const health = await app.inject({ method: 'GET', url: '/health' });
    assert.equal(health.statusCode, 503);

    const livez = await app.inject({ method: 'GET', url: '/livez' });
    assert.equal(livez.statusCode, 200);
    assert.equal((livez.json() as { status: string }).status, 'ok');
  } finally {
    release();
    await hung;
    await app.close();
  }
});

test('/livez is 200 when idle', async () => {
  const app = buildServer({ liveness: new ScrapeLiveness(50) });
  try {
    const res = await app.inject({ method: 'GET', url: '/livez' });
    assert.equal(res.statusCode, 200);
  } finally {
    await app.close();
  }
});

test('an abandoned scrape stops counting toward stuck', async () => {
  const liveness = new ScrapeLiveness(20);
  const app = buildServer({ liveness });
  const gone = new AbortController();
  let release: () => void = () => {};
  const hung = liveness.track(
    () => new Promise<void>((r) => (release = r)),
    gone.signal
  );
  try {
    await new Promise((r) => setTimeout(r, 40));
    assert.equal(
      (await app.inject({ method: 'GET', url: '/health' })).statusCode,
      503
    );

    // Caller hung up: the work is still running but nobody is waiting on it, so
    // it must not keep aging on the liveness books.
    gone.abort();
    const res = await app.inject({ method: 'GET', url: '/health' });
    assert.equal(res.statusCode, 200);
    const body = res.json() as HealthBody;
    assert.equal(body.status, 'ok');
    assert.equal(body.scrapeInFlight, 0);
  } finally {
    release();
    await hung;
    await app.close();
  }
});

/**
 * Regression test for the wiring, not just the tracker.
 *
 * The first cut of this listened on request.raw, whose 'close' fires once the
 * request body has been read (~1ms in for these small JSON bodies). That aborted
 * every scrape immediately and silently disabled stuck detection entirely, while
 * the tracker's own unit tests still passed. These drive real sockets.
 */
test('clientGoneSignal does not fire when the client waits for the response', async () => {
  const app = Fastify();
  let abortedDuringHandler = false;
  app.post('/slow', async (_req, reply) => {
    const gone = clientGoneSignal(reply);
    await new Promise((r) => setTimeout(r, 150));
    abortedDuringHandler = gone.aborted;
    return { ok: true };
  });
  await app.listen({ port: 0, host: '127.0.0.1' });
  const { port } = app.server.address() as AddressInfo;
  try {
    await fetch(`http://127.0.0.1:${port}/slow`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: '{}',
    });
    assert.equal(abortedDuringHandler, false);
  } finally {
    await app.close();
  }
});

test('clientGoneSignal fires when the client hangs up early', async () => {
  const app = Fastify();
  let abortedDuringHandler: boolean | null = null;
  app.post('/slow', async (_req, reply) => {
    const gone = clientGoneSignal(reply);
    await new Promise((r) => setTimeout(r, 300));
    abortedDuringHandler = gone.aborted;
    return { ok: true };
  });
  await app.listen({ port: 0, host: '127.0.0.1' });
  const { port } = app.server.address() as AddressInfo;
  try {
    const ac = new AbortController();
    setTimeout(() => ac.abort(), 80);
    await assert.rejects(
      fetch(`http://127.0.0.1:${port}/slow`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: '{}',
        signal: ac.signal,
      })
    );
    await new Promise((r) => setTimeout(r, 400));
    assert.equal(abortedDuringHandler, true);
  } finally {
    await app.close();
  }
});
