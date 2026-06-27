/**
 * The /metrics endpoint exposes Prometheus exposition text, including the default
 * Node process metrics and the scraper-specific in-flight gauge. These drive
 * buildServer via Fastify's inject so nothing touches the network.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';

import { buildServer } from '../src/index.js';
import { ScrapeLiveness } from '../src/health.js';

test('/metrics serves Prometheus exposition with default and custom series', async () => {
  const app = buildServer({ liveness: new ScrapeLiveness(50) });
  try {
    const res = await app.inject({ method: 'GET', url: '/metrics' });
    assert.equal(res.statusCode, 200);
    assert.match(res.headers['content-type'] ?? '', /text\/plain/);
    const body = res.body;
    assert.match(body, /process_cpu_seconds_total/); // default node metric
    assert.match(body, /jsa_scraper_in_flight/); // custom gauge
    assert.match(
      body,
      /jsa_scraper_scrapes_total|jsa_scraper_scrape_duration_seconds/
    );
  } finally {
    await app.close();
  }
});
