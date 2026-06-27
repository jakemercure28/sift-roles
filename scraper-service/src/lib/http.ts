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

export async function safeFetch(
  url: string,
  options: RequestInit = {},
  label = ''
): Promise<Response | null> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), FETCH_TIMEOUT_MS);
  try {
    const res = await fetch(url, { ...options, signal: controller.signal });
    if (!res.ok) {
      // Surface blocking/throttling responses (429/403/5xx). 404 and other
      // expected misses stay quiet so URL/board probing doesn't spam the logs.
      if (res.status === 429 || res.status === 403 || res.status >= 500) {
        warn('Fetch blocked', { label, status: res.status });
      }
      return null;
    }
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
