import Fastify from 'fastify';

import type { ScrapeRequestBody, ScrapeResponse } from './types.js';
import { runScrape, listPlatforms, onboarded } from './bridge.js';
import { setProfileDir } from './lib/interop.js';
import { ScrapeLiveness } from './health.js';
import {
  registry as metricsRegistry,
  observeScrape,
  setInFlightSource,
} from './metrics.js';

const PORT = Number(process.env.SCRAPER_PORT ?? 4040);
const HOST = process.env.SCRAPER_HOST ?? '0.0.0.0';

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

  app.post<{ Body: ScrapeRequestBody }>('/scrape', async (request) => {
    const requested = request.body?.platforms;
    // Point the worker at the caller's tenant profile dir for this request
    // (resets to DATA_DIR when omitted). /scrape is serialized by the Go backend.
    setProfileDir(request.body?.profileDir);
    const startedAt = Date.now();
    let jobs;
    try {
      jobs = await liveness.track(() => runScrape(requested));
    } catch (err) {
      observeScrape('error', (Date.now() - startedAt) / 1000);
      throw err;
    }
    observeScrape('ok', (Date.now() - startedAt) / 1000, jobs.length);
    const response: ScrapeResponse = {
      count: jobs.length,
      platforms: requested?.length ? requested : listPlatforms(),
      jobs,
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
