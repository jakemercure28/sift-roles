/**
 * HTTP helpers, ported from lib/utils.js. safeFetch applies the same 12s
 * AbortController timeout and the same quiet-on-404 / warn-on-block policy, and
 * returns null (never throws) so the company loop can count failures.
 */
import { FETCH_TIMEOUT_MS } from './config.js';
import { warn } from './log.js';

export function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

/**
 * Per-host politeness pacer for board-mode scraping. Board mode fetches the
 * cross-tenant board union in one pass, which can put thousands of requests on
 * a single ATS API host (all Greenhouse boards share boards-api.greenhouse.io),
 * so request STARTS to the same host are spaced at least minIntervalMs apart
 * while different hosts (e.g. each Workday tenant's own domain) proceed
 * independently. The per-tenant scrape path keeps its batch+sleep pacing
 * unchanged.
 */
export class HostPacer {
  private next = new Map<string, number>();

  constructor(private minIntervalMs: number) {}

  /** Resolves when a request to url's host may start. */
  async wait(url: string): Promise<void> {
    if (this.minIntervalMs <= 0) return;
    let host: string;
    try {
      host = new URL(url).hostname;
    } catch {
      return;
    }
    const now = Date.now();
    const at = Math.max(this.next.get(host) ?? 0, now);
    this.next.set(host, at + this.minIntervalMs);
    if (at > now) await sleep(at - now);
  }
}

function hostOf(url: string): string | null {
  try {
    return new URL(url).hostname;
  } catch {
    return null;
  }
}

/** Base cooldown applied to a host on its first 429, doubled per repeat. */
const COOLDOWN_BASE_MS = 60_000;
const COOLDOWN_MAX_MS = 15 * 60_000;

/**
 * Per-host 429 circuit breaker.
 *
 * Pacing alone does not help against a host that is already refusing us: the
 * Workday path fans out WORKDAY_CONCURRENCY companies x DETAIL_CONCURRENCY
 * detail fetches, and on a throttling tenant every one of those came back 429
 * and the loop immediately fired the next. A single prod pass logged 3,896
 * Workday 429s, which both wasted the pass's time budget and pushed the pass
 * past the timeouts that were getting the worker killed.
 *
 * On a 429 the host is parked for a cooldown (Retry-After when the server sends
 * one, else exponential from COOLDOWN_BASE_MS) and `blocked()` short-circuits
 * further requests to it instead of spending a round trip to be refused again.
 */
export class HostBreaker {
  private until = new Map<string, number>();
  private strikes = new Map<string, number>();

  /** True when host is parked and the caller should skip the request. */
  blocked(url: string, now: number = Date.now()): boolean {
    const host = hostOf(url);
    if (host === null) return false;
    const until = this.until.get(host);
    if (until === undefined) return false;
    if (now >= until) {
      this.until.delete(host);
      return false;
    }
    return true;
  }

  /** Park url's host after a 429. `retryAfter` is the raw header, if present. */
  trip(
    url: string,
    retryAfter?: string | null,
    now: number = Date.now()
  ): void {
    const host = hostOf(url);
    if (host === null) return;
    const strikes = (this.strikes.get(host) ?? 0) + 1;
    this.strikes.set(host, strikes);
    const backoff = Math.min(
      COOLDOWN_BASE_MS * 2 ** (strikes - 1),
      COOLDOWN_MAX_MS
    );
    // Retry-After is seconds or an HTTP date; honor it when it parses and asks
    // for longer than our own backoff.
    let hinted = 0;
    if (retryAfter) {
      const secs = Number(retryAfter);
      hinted = Number.isFinite(secs)
        ? secs * 1000
        : Math.max(0, Date.parse(retryAfter) - now);
    }
    this.until.set(
      host,
      now + Math.min(Math.max(backoff, hinted), COOLDOWN_MAX_MS)
    );
  }

  /** Clear a host's cooldown and strike count after a clean response. */
  clear(url: string): void {
    const host = hostOf(url);
    if (host === null) return;
    this.until.delete(host);
    this.strikes.delete(host);
  }
}

// undici's default `accept-language: *` trips WAFs on some ATS hosts (Workday
// tenants like Nordstrom/Regions/GEICO answer HTTP 500 to it); a real language
// tag and browser user-agent get a normal 200 from the same endpoints.
const DEFAULT_HEADERS: Record<string, string> = {
  'accept-language': 'en-US,en;q=0.9',
  'user-agent':
    'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36',
};

/** Outcome of a conditional (If-None-Match) fetch. */
export interface ConditionalResult {
  /** The 200 response, null on failure or 304. */
  res: Response | null;
  /** True when the server answered 304 Not Modified. */
  notModified: boolean;
}

/**
 * Like safeFetch but sends If-None-Match when an etag is known and surfaces
 * 304 Not Modified as its own outcome (safeFetch folds every non-2xx into
 * null, which would make "unchanged" indistinguishable from "blocked").
 */
export async function conditionalFetch(
  url: string,
  etag: string | undefined,
  label = ''
): Promise<ConditionalResult> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), FETCH_TIMEOUT_MS);
  try {
    const headers: Record<string, string> = { ...DEFAULT_HEADERS };
    if (etag) headers['if-none-match'] = etag;
    const res = await fetch(url, { headers, signal: controller.signal });
    if (res.status === 304) return { res: null, notModified: true };
    if (!res.ok) {
      if (res.status === 429 || res.status === 403 || res.status >= 500) {
        warn('Fetch blocked', { label, status: res.status });
      }
      return { res: null, notModified: false };
    }
    return { res, notModified: false };
  } catch (err) {
    warn('Fetch error', {
      label,
      error: err instanceof Error ? err.message : String(err),
    });
    return { res: null, notModified: false };
  } finally {
    clearTimeout(timer);
  }
}

export async function safeFetch(
  url: string,
  options: RequestInit = {},
  label = '',
  breaker?: HostBreaker
): Promise<Response | null> {
  // Host is already known to be refusing us: skip the round trip entirely.
  if (breaker?.blocked(url)) return null;
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), FETCH_TIMEOUT_MS);
  try {
    const res = await fetch(url, {
      ...options,
      headers: { ...DEFAULT_HEADERS, ...(options.headers as object) },
      signal: controller.signal,
    });
    if (!res.ok) {
      // Surface blocking/throttling responses (429/403/5xx). 404 and other
      // expected misses stay quiet so URL/board probing doesn't spam the logs.
      if (res.status === 429 || res.status === 403 || res.status >= 500) {
        warn('Fetch blocked', { label, status: res.status });
      }
      if (res.status === 429) {
        breaker?.trip(url, res.headers.get('retry-after'));
      }
      return null;
    }
    breaker?.clear(url);
    return res;
  } catch (err) {
    warn('Fetch error', {
      label,
      error: err instanceof Error ? err.message : String(err),
    });
    return null;
  } finally {
    clearTimeout(timer);
  }
}
