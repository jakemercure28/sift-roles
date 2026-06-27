import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  JOB_ANALYZE_KEYWORDS,
  analyzeKeywordsPayload,
  createProcessor,
  loadQueueRuntimeConfig,
  redisConnectionOptions,
} from '../src/queue/index.js';
import { traceIDFromJobPayload } from '../src/queue-worker.js';

test('processor dispatches analyzeKeywords jobs without Redis', async () => {
  const processor = createProcessor({
    async analyzeKeywords(payload) {
      return { keywordCount: payload.keywords.length };
    },
  });

  const result = await processor({
    name: JOB_ANALYZE_KEYWORDS,
    data: { keywords: ['sre', 'platform'] },
  } as never);

  assert.deepEqual(result, { keywordCount: 2 });
});

test('analyzeKeywords payload can carry trace_id metadata', () => {
  assert.deepEqual(analyzeKeywordsPayload(['sre'], ' abc123 '), {
    keywords: ['sre'],
    trace_id: 'abc123',
  });
  assert.deepEqual(analyzeKeywordsPayload(['sre'], '   '), {
    keywords: ['sre'],
  });
});

test('processor rejects unknown job names without Redis', async () => {
  const processor = createProcessor();

  await assert.rejects(
    processor({
      name: 'scrape',
      data: { keywords: [] },
    } as never),
    /unknown BullMQ job: scrape/
  );
});

test('queue config uses Redis env vars and concurrency defaults', () => {
  const config = loadQueueRuntimeConfig({
    REDIS_ADDR: 'redis:6380',
    REDIS_PASSWORD: 'secret',
  });

  assert.equal(config.redisAddr, 'redis:6380');
  assert.equal(config.redisPassword, 'secret');
  assert.equal(config.workerConcurrency, 2);
  assert.deepEqual(redisConnectionOptions(config), {
    host: 'redis',
    port: 6380,
    password: 'secret',
    maxRetriesPerRequest: null,
  });
});

test('traceIDFromJobPayload parses optional trace metadata', () => {
  assert.equal(traceIDFromJobPayload({ trace_id: ' abc123 ' }), 'abc123');
  assert.equal(traceIDFromJobPayload({}), undefined);
  assert.equal(traceIDFromJobPayload({ trace_id: '' }), undefined);
  assert.equal(traceIDFromJobPayload({ trace_id: 123 }), undefined);
  assert.equal(traceIDFromJobPayload(null), undefined);
});
