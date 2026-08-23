/**
 * Work-health tracking for the scraper worker.
 *
 * A shallow "process is up" answer hides the failure that actually matters: a
 * scrape that leaks/hangs while Fastify's event loop stays responsive. There, a
 * plain liveness answer stays 200 and nothing ever surfaces the hang.
 *
 * ScrapeLiveness closes that gap. Each scrape's start time is recorded while it
 * runs and cleared when it settles, so a scrape stuck past the threshold leaves
 * a stale start time behind. GET /health reports `stuck` off that and returns
 * 503.
 *
 * /health is a *reporting* endpoint, not a restart trigger. The kubelet probes
 * the shallow GET /livez instead. Restarting the container does not un-stick a
 * slow board pass, it just discards the pass's work, so `stuck` is something to
 * observe and alert on rather than something to kill the process over.
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
  // Entry objects rather than raw timestamps: two scrapes starting in the same
  // millisecond would collide under indexOf on a number[].
  private readonly starts = new Set<{ at: number }>();
  private lastSuccessAt: number | null = null;

  constructor(thresholdMs: number = DEFAULT_STUCK_MS) {
    this.thresholdMs = thresholdMs;
  }

  /**
   * Run `fn` while counting it as an in-flight scrape. The start time is recorded
   * on entry and removed once `fn` settles (resolve or reject), so a hung `fn`
   * leaves its start time behind for `snapshot` to flag.
   *
   * When `signal` aborts, the entry is dropped immediately even though `fn` may
   * still be running. That is deliberate: the Go client gives up on a chunk at
   * SCRAPE_TIMEOUT_MINUTES (default 10) while the stuck threshold here is 15, so
   * work the caller already walked away from used to keep aging on our books and
   * drive the service to `degraded` for a chunk nobody was waiting on.
   */
  async track<T>(fn: () => Promise<T>, signal?: AbortSignal): Promise<T> {
    const entry = { at: Date.now() };
    this.starts.add(entry);
    const abandon = () => this.starts.delete(entry);
    signal?.addEventListener('abort', abandon, { once: true });
    try {
      const result = await fn();
      this.lastSuccessAt = Date.now();
      return result;
    } finally {
      this.starts.delete(entry);
      signal?.removeEventListener('abort', abandon);
    }
  }

  snapshot(now: number = Date.now()): LivenessSnapshot {
    const oldestStart =
      this.starts.size > 0
        ? Math.min(...[...this.starts].map((e) => e.at))
        : null;
    const oldestAgeMs = oldestStart === null ? null : now - oldestStart;
    return {
      inFlight: this.starts.size,
      oldestAgeMs,
      lastSuccessAt: this.lastSuccessAt,
      stuck: oldestAgeMs !== null && oldestAgeMs > this.thresholdMs,
    };
  }
}
