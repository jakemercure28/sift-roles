import Fastify from 'fastify';

import type { ScrapeRequestBody, ScrapeResponse } from './types.js';
import {
  runScrape,
  runBoardScrape,
  listPlatforms,
  onboarded,
} from './bridge.js';
import { setProfileDir } from './lib/interop.js';
import { ScrapeLiveness } from './health.js';
import {
  registry as metricsRegistry,
  observeScrape,
  setInFlightSource,
} from './metrics.js';

const PORT = Number(process.env.SCRAPER_PORT ?? 4040);
const HOST = process.env.SCRAPER_HOST ?? '0.0.0.0';

/**
 * Signal that fires when the client walks away before we answer.
 *
 * Deliberately watches the reply stream, not request.raw: since Node 16
 * IncomingMessage emits 'close' once the request body has been read, which for
 * these small JSON bodies is ~1ms into the handler. Listening there aborts every
 * scrape immediately and silently disables stuck detection. A reply stream that
 * closes with writableFinished false is a real disconnect.
 */
export function clientGoneSignal(reply: {
  raw: { on(ev: 'close', cb: () => void): unknown; writableFinished: boolean };
}): AbortSignal {
  const gone = new AbortController();
  reply.raw.on('close', () => {
    if (!reply.raw.writableFinished) gone.abort();
  });
  return gone.signal;
}

export function buildServer(opts: { liveness?: ScrapeLiveness } = {}) {
  const app = Fastify({
    logger: {
      // Emit the level as its string name ("info"/"warn"/"error") instead of
      // pino's default numeric code (30/40/50) so every service in the stack
      // shares one level vocabulary in Loki. Matches Go slog and lib/log.
      formatters: {
        level(label) {
          return { level: label };
        },
      },
    },
  });
  const liveness = opts.liveness ?? new ScrapeLiveness();
  setInFlightSource(() => liveness.snapshot().inFlight);

  // /health (Docker healthcheck) and /metrics (Prometheus) are polled roughly
  // once per second. Logging every request at info buries the real scrape logs
  // (~46k access lines per 12h), so quiet their request/response logging to warn.
  const pollRoute = { logLevel: 'warn' as const };

  app.get('/metrics', pollRoute, async (_request, reply) => {
    reply.header('Content-Type', metricsRegistry.contentType);
    return metricsRegistry.metrics();
  });

  // Process liveness, deliberately shallow: answering at all proves the event
  // loop is not wedged, which is the only thing a restart can fix. This is what
  // the kubelet probes. /health below reports *work* health and can go 503
  // while the process is perfectly fine, so probing it conflated "a board pass
  // is taking a while" with "this container is broken" and killed the pod
  // mid-pass every few hours.
  app.get('/livez', pollRoute, async () => ({ status: 'ok' }));

  app.get('/health', pollRoute, async (_request, reply) => {
    const live = liveness.snapshot();
    if (live.stuck) reply.code(503);
    return {
      status: live.stuck ? 'degraded' : 'ok',
      onboarded: onboarded(),
      platforms: listPlatforms(),
      scrapeInFlight: live.inFlight,
      oldestScrapeMs: live.oldestAgeMs,
      lastSuccessAt: live.lastSuccessAt,
    };
  });

  app.post<{ Body: ScrapeRequestBody }>('/scrape', async (request, reply) => {
    const requested = request.body?.platforms;
    const boards = request.body?.boards;
    const boardMode = Array.isArray(boards) && boards.length > 0;
    // Point the worker at the caller's tenant profile dir for this request
    // (resets to DATA_DIR when omitted). /scrape is serialized by the Go
    // backend. Board mode reads no profile files at all.
    if (!boardMode) setProfileDir(request.body?.profileDir);
    const startedAt = Date.now();
    // The caller hanging up (its own SCRAPE_TIMEOUT_MINUTES, or a redeploy)
    // means nobody is waiting on this scrape any more. We cannot cancel the
    // underlying CJS scrapers, but we can stop the abandoned work from aging on
    // the liveness books and dragging /health to degraded.
    const gone = clientGoneSignal(reply);
    let jobs;
    let boardResults;
    try {
      if (boardMode) {
        const report = await liveness.track(() => runBoardScrape(boards), gone);
        jobs = report.jobs;
        boardResults = report.boards;
      } else {
        jobs = await liveness.track(() => runScrape(requested), gone);
      }
    } catch (err) {
      observeScrape('error', (Date.now() - startedAt) / 1000);
      throw err;
    }
    observeScrape('ok', (Date.now() - startedAt) / 1000, jobs.length);
    const platforms = boardMode
      ? [...new Set(boards.map((b) => b.platform))]
      : requested?.length
        ? requested
        : listPlatforms();
    const response: ScrapeResponse = {
      count: jobs.length,
      platforms,
      jobs,
      ...(boardResults ? { boards: boardResults } : {}),
    };
    return response;
  });

  return app;
}

// Start only when run directly (not when imported by tests).
const isMain = import.meta.url === `file://${process.argv[1]}`;
if (isMain) {
  const app = buildServer();
  app
    .listen({ port: PORT, host: HOST })
    .then((addr) => app.log.info(`scraper-service listening on ${addr}`))
    .catch((err) => {
      app.log.error(err);
      process.exit(1);
    });
}
