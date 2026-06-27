import type { ConnectionOptions } from 'bullmq';

export const TS_QUEUE_NAME = 'ts:jobs';

export interface QueueRuntimeConfig {
  readonly redisAddr: string;
  readonly redisPassword?: string;
  readonly workerConcurrency: number;
}

function envInt(value: string | undefined, fallback: number): number {
  if (!value) {
    return fallback;
  }
  const parsed = Number.parseInt(value, 10);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : fallback;
}

export function loadQueueRuntimeConfig(
  env: NodeJS.ProcessEnv = process.env
): QueueRuntimeConfig {
  return {
    redisAddr: env.REDIS_ADDR || 'localhost:6379',
    redisPassword: env.REDIS_PASSWORD || undefined,
    workerConcurrency: env.TS_QUEUE_CONCURRENCY
      ? envInt(env.TS_QUEUE_CONCURRENCY, 2)
      : envInt(env.QUEUE_CONCURRENCY, 2),
  };
}

export function redisConnectionOptions(
  config: QueueRuntimeConfig = loadQueueRuntimeConfig()
): ConnectionOptions {
  const lastColon = config.redisAddr.lastIndexOf(':');
  const host =
    lastColon > 0 ? config.redisAddr.slice(0, lastColon) : config.redisAddr;
  const port =
    lastColon > 0
      ? Number.parseInt(config.redisAddr.slice(lastColon + 1), 10)
      : 6379;

  return {
    host,
    port: Number.isFinite(port) ? port : 6379,
    password: config.redisPassword,
    maxRetriesPerRequest: null,
  };
}
