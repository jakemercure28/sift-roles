/**
 * Title filter, ported from lib/scraper-utils.js.
 *
 * Multi-word terms ("financial analyst") and longer single words (>= 6 chars,
 * e.g. "investment", "marketing") match as substrings so plurals/suffixes still
 * count. Short single words ("risk", "tax", "vp") must match on a word boundary
 * so they don't fire inside unrelated words (e.g. "tax" in "syntax").
 */
import { companyConfig } from './config.js';

export function matchesSearchTerms(
  text: string,
  searchTerms: string[] = companyConfig().SEARCH_TERMS
): boolean {
  if (!text) return false;
  const lower = text.toLowerCase();
  const words = lower.split(/[^a-z0-9]+/).filter(Boolean);
  return searchTerms.some((raw) => {
    const term = String(raw || '')
      .toLowerCase()
      .trim();
    if (!term) return false;
    if (term.includes(' ') || term.length >= 6) return lower.includes(term);
    return words.includes(term);
  });
}
