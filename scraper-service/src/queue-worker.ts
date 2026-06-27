import {
  TS_QUEUE_NAME,
  createJobsWorker,
  loadQueueRuntimeConfig,
} from './queue/index.js';

export function traceIDFromJobPayload(data: unknown): string | undefined {
  if (!data || typeof data !== 'object' || !('trace_id' in data)) {
    return undefined;
  }
  const traceID = (data as { trace_id?: unknown }).trace_id;
  if (typeof traceID !== 'string' || !traceID.trim()) {
    return undefined;
  }
  return traceID.trim();
}

export function startQueueWorker() {
  const config = loadQueueRuntimeConfig();
  const worker = createJobsWorker(config);

  worker.on('completed', (job, result) => {
    console.log(
      JSON.stringify({
        event: 'completed',
        queue: TS_QUEUE_NAME,
        jobId: job.id,
        jobName: job.name,
        trace_id: traceIDFromJobPayload(job.data),
        result,
      })
    );
  });

  worker.on('failed', (job, error) => {
    console.error(
      JSON.stringify({
        event: 'failed',
        queue: TS_QUEUE_NAME,
        jobId: job?.id,
        jobName: job?.name,
        trace_id: traceIDFromJobPayload(job?.data),
        error: error.message,
      })
    );
  });

  worker.on('error', (error) => {
    console.error(
      JSON.stringify({
        event: 'error',
        queue: TS_QUEUE_NAME,
        error: error.message,
      })
    );
  });

  console.log(
    JSON.stringify({
      event: 'started',
      queue: TS_QUEUE_NAME,
      redisAddr: config.redisAddr,
      concurrency: config.workerConcurrency,
    })
  );

  async function shutdown(signal: NodeJS.Signals) {
    console.log(
      JSON.stringify({ event: 'stopping', signal, queue: TS_QUEUE_NAME })
    );
    await worker.close();
  }

  process.once('SIGINT', (signal) => {
    shutdown(signal)
      .then(() => process.exit(0))
      .catch((error) => {
        console.error(error);
        process.exit(1);
      });
  });

  process.once('SIGTERM', (signal) => {
    shutdown(signal)
      .then(() => process.exit(0))
      .catch((error) => {
        console.error(error);
        process.exit(1);
      });
  });

  return worker;
}

const isMain = import.meta.url === `file://${process.argv[1]}`;
if (isMain) {
  startQueueWorker();
}
