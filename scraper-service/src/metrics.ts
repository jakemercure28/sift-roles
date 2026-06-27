/**
 * Prometheus instrumentation for the scraper-service.
 *
 * Exposes default Node process/runtime metrics plus a few scrape-specific series
 * (duration, result counts, jobs returned, in-flight gauge) on its own registry so
 * the Go backend's Prometheus can scrape GET /metrics. All series are prefixed
 * jsa_scraper_ to group cleanly alongside the Go backend's jsa_ metrics.
 */
import {
  collectDefaultMetrics,
  Counter,
  Gauge,
  Histogram,
  Registry,
} from 'prom-client';

export const registry = new Registry();
collectDefaultMetrics({ register: registry });

const scrapeDuration = new Histogram({
  name: 'jsa_scraper_scrape_duration_seconds',
  help: 'Duration of one /scrape request in seconds.',
  buckets: [1, 2, 5, 10, 30, 60, 120, 300, 600],
  registers: [registry],
});

const scrapesTotal = new Counter({
  name: 'jsa_scraper_scrapes_total',
  help: 'Scrape requests completed, labeled by result (ok|error).',
  labelNames: ['result'] as const,
  registers: [registry],
});

const jobsReturned = new Counter({
  name: 'jsa_scraper_jobs_returned_total',
  help: 'Total job leads returned across all scrapes.',
  registers: [registry],
});

// Pull-based: the gauge reads its value from the liveness source at collect time,
// so it always reflects the current in-flight count without separate bookkeeping.
let inFlightSource: (() => number) | null = null;
new Gauge({
  name: 'jsa_scraper_in_flight',
  help: 'Scrapes currently in flight.',
  registers: [registry],
  collect() {
    if (inFlightSource) this.set(inFlightSource());
  },
});

/** observeScrape records one settled /scrape request. */
export function observeScrape(
  result: 'ok' | 'error',
  seconds: number,
  jobCount = 0
): void {
  scrapesTotal.labels(result).inc();
  scrapeDuration.observe(seconds);
  if (jobCount > 0) jobsReturned.inc(jobCount);
}

/** setInFlightSource wires the in-flight gauge to a liveness snapshot reader. */
export function setInFlightSource(fn: () => number): void {
  inFlightSource = fn;
}
