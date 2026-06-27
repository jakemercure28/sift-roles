/**
 * Work-health behavior for the scraper-service /health endpoint.
 *
 * The point of ScrapeLiveness is that a scrape stuck past the threshold flips
 * /health to 503 so Docker restarts the container, while a normal scrape that
 * settles leaves health green. These tests inject a tracker with a tiny threshold
 * and drive buildServer via Fastify's inject, so nothing waits on real minutes or
 * touches the network.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';

import { buildServer } from '../src/index.js';
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
