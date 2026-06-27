/**
 * Scraper timing + content limits. Ported from the former root config/constants.js
 * (which also held Gemini and dashboard settings owned by the Go side). Only the
 * values the scrapers actually read live here; they are fixed literals, matching
 * the CJS original.
 */

// HTTP fetch default timeout.
export const FETCH_TIMEOUT_MS = 12_000;

// Scraper politeness delays (ms between requests to the same host).
export const SCRAPER_DELAY_MS = 300;
export const SCRAPER_DELAY_SLOW_MS = 400; // Workable needs more time
export const SCRAPER_DELAY_RSS_MS = 500;

// Max job description length kept before truncation.
export const MAX_DESCRIPTION_LENGTH = 15_000;
