import { Queue, Worker, type JobsOptions } from 'bullmq';

import {
  JOB_ANALYZE_KEYWORDS,
  type QueueJobName,
  type QueueJobPayload,
  type QueueJobResult,
  createProcessor,
} from './jobs.js';
import {
  TS_QUEUE_NAME,
  loadQueueRuntimeConfig,
  redisConnectionOptions,
  type QueueRuntimeConfig,
} from './config.js';

export function createJobsQueue(
  config: QueueRuntimeConfig = loadQueueRuntimeConfig()
) {
  return new Queue<QueueJobPayload, QueueJobResult, QueueJobName>(
    TS_QUEUE_NAME,
    {
      connection: redisConnectionOptions(config),
    }
  );
}

export function createJobsWorker(
  config: QueueRuntimeConfig = loadQueueRuntimeConfig()
) {
  return new Worker<QueueJobPayload, QueueJobResult, QueueJobName>(
    TS_QUEUE_NAME,
    createProcessor(),
    {
      connection: redisConnectionOptions(config),
      concurrency: config.workerConcurrency,
    }
  );
}

export function analyzeKeywordsPayload(
  keywords: readonly string[],
  traceID?: string
): QueueJobPayload {
  const payload: QueueJobPayload = { keywords };
  if (traceID && traceID.trim()) {
    return { ...payload, trace_id: traceID.trim() };
  }
  return payload;
}

export async function enqueueAnalyzeKeywords(
  queue: Queue<QueueJobPayload, QueueJobResult, QueueJobName>,
  keywords: readonly string[],
  traceID?: string,
  options?: JobsOptions
) {
  return queue.add(
    JOB_ANALYZE_KEYWORDS,
    analyzeKeywordsPayload(keywords, traceID),
    options
  );
}

export { JOB_ANALYZE_KEYWORDS, createProcessor } from './jobs.js';
export {
  TS_QUEUE_NAME,
  loadQueueRuntimeConfig,
  redisConnectionOptions,
} from './config.js';
