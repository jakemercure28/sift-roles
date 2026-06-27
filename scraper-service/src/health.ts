/**
 * Work-health tracking for the scraper worker.
 *
 * The Docker healthcheck hits GET /health every 30s, but a shallow "process is
 * up" answer hides the failure that actually matters: a Playwright scrape that
 * leaks/hangs while Fastify's event loop stays responsive. There, /health would
 * keep returning 200 and Docker would never restart the container.
 *
 * ScrapeLiveness closes that gap. Each scrape's start time is recorded while it
 * runs and cleared when it settles, so a scrape stuck past the threshold leaves a
 * stale start time behind. /health reports `stuck` off that, returns 503, and the
 * existing healthcheck (3 retries) restarts the container.
 *
 * The Go backend serializes /scrape today, but we track a multiset of start times
 * rather than a single flag so overlapping scrapes never lose each other's state.
 */

const DEFAULT_STUCK_MS =
  Number(process.env.SCRAPE_STUCK_MINUTES ?? 15) * 60_000;

export interface LivenessSnapshot {
  /** Number of scrapes currently in flight. */
  inFlight: number;
  /** Age of the oldest in-flight scrape in ms, or null when idle. */
  oldestAgeMs: number | null;
  /** Epoch ms of the last scrape that settled successfully, or null. */
  lastSuccessAt: number | null;
  /** True when the oldest in-flight scrape has overrun the stuck threshold. */
  stuck: boolean;
}

export class ScrapeLiveness {
  private readonly thresholdMs: number;
  private readonly starts: number[] = [];
  private lastSuccessAt: number | null = null;

  constructor(thresholdMs: number = DEFAULT_STUCK_MS) {
    this.thresholdMs = thresholdMs;
  }

  /**
   * Run `fn` while counting it as an in-flight scrape. The start time is recorded
   * on entry and removed once `fn` settles (resolve or reject), so a hung `fn`
   * leaves its start time behind for `snapshot` to flag.
   */
  async track<T>(fn: () => Promise<T>): Promise<T> {
    const startedAt = Date.now();
    this.starts.push(startedAt);
    try {
      const result = await fn();
      this.lastSuccessAt = Date.now();
      return result;
    } finally {
      const i = this.starts.indexOf(startedAt);
      if (i !== -1) this.starts.splice(i, 1);
    }
  }

  snapshot(now: number = Date.now()): LivenessSnapshot {
    const oldestStart =
      this.starts.length > 0 ? Math.min(...this.starts) : null;
    const oldestAgeMs = oldestStart === null ? null : now - oldestStart;
    return {
      inFlight: this.starts.length,
      oldestAgeMs,
      lastSuccessAt: this.lastSuccessAt,
      stuck: oldestAgeMs !== null && oldestAgeMs > this.thresholdMs,
    };
  }
}
