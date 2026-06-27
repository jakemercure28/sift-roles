/**
 * Company config for the scrapers. Loads the active profile's companies.json
 * (the company lists + search terms the setup wizard writes), then folds in any
 * Gemini-discovered companies from DATA_DIR/suggested-companies.json. Cached
 * per active profile dir and refreshed when either config file changes.
 *
 * Timing constants are re-exported from ./constants.js so the scrapers keep
 * importing them from './config.js' as before.
 */
import { readFileSync, statSync } from 'node:fs';
import { resolve } from 'node:path';
import { getProfileDir, readProfileJSON } from './interop.js';
import { deriveSearchTermsFromResume } from './resume_terms.js';

export {
  FETCH_TIMEOUT_MS,
  SCRAPER_DELAY_MS,
  SCRAPER_DELAY_SLOW_MS,
  SCRAPER_DELAY_RSS_MS,
  MAX_DESCRIPTION_LENGTH,
} from './constants.js';

/** A Workday board (tenant + workday cluster number + board path). */
interface WorkdayEntry {
  sub: string;
  wd: number;
  board: string;
}

/** Merged company config (profile + Gemini suggestions). */
export interface CompanyConfig {
  SEARCH_TERMS: string[];
  GREENHOUSE_COMPANIES: string[];
  LEVER_COMPANIES: string[];
  ASHBY_COMPANIES: string[];
  WORKABLE_COMPANIES: string[];
  WORKDAY_COMPANIES: WorkdayEntry[];
  RIPPLING_COMPANIES: string[];
  [key: string]: unknown;
}

interface Suggested {
  greenhouse: string[];
  ashby: string[];
  lever: string[];
  workday: WorkdayEntry[];
}

// A Workday board is uniquely identified by its tenant AND board path: one tenant
// can host several brand boards, so dedup keys off this pair. Board paths are
// case-sensitive and not normalized.
function workdayKey(entry: WorkdayEntry | undefined): string | null {
  return entry && entry.sub && entry.board
    ? `${entry.sub}/${entry.board}`
    : null;
}

function loadSuggested(profileDir: string): Suggested {
  try {
    const raw = readFileSync(
      resolve(profileDir, 'suggested-companies.json'),
      'utf8'
    );
    const data = JSON.parse(raw) as Partial<Suggested>;
    return {
      greenhouse: Array.isArray(data.greenhouse) ? data.greenhouse : [],
      ashby: Array.isArray(data.ashby) ? data.ashby : [],
      lever: Array.isArray(data.lever) ? data.lever : [],
      workday: Array.isArray(data.workday) ? data.workday : [],
    };
  } catch {
    return { greenhouse: [], ashby: [], lever: [], workday: [] };
  }
}

const addNew = (arr: string[], extra: string[]): string[] =>
  extra.length ? [...new Set([...arr, ...extra])] : arr;

const mergeWorkday = (
  base: WorkdayEntry[],
  extra: WorkdayEntry[]
): WorkdayEntry[] => {
  if (!extra || !extra.length) return base;
  const seen = new Set((base || []).map(workdayKey).filter(Boolean));
  const additions = extra.filter(
    (e) => e && e.sub && e.wd && e.board && !seen.has(workdayKey(e))
  );
  return additions.length ? [...(base || []), ...additions] : base || [];
};

let cached: CompanyConfig | undefined;
let cachedDir: string | undefined;
let cachedSignature: string | undefined;

function fileSignature(profileDir: string, filename: string): string {
  try {
    const stat = statSync(resolve(profileDir, filename), { bigint: true });
    return `${stat.size}:${stat.mtimeNs}`;
  } catch {
    return 'missing';
  }
}

function configSignature(profileDir: string): string {
  return [
    fileSignature(profileDir, 'companies.json'),
    fileSignature(profileDir, 'suggested-companies.json'),
    fileSignature(profileDir, 'resume.md'),
  ].join('|');
}

/**
 * The active profile's merged company config. Cached per profile dir: hosted
 * Postgres can switch the active tenant dir between /scrape requests (serialized
 * by Go), so the cache re-reads when the dir changes instead of pinning the
 * first tenant's config for the life of the process. The same profile dir can
 * also gain new Gemini suggestions between requests, so file metadata is part
 * of the cache key.
 */
export function companyConfig(): CompanyConfig {
  const profileDir = getProfileDir();
  const signature = configSignature(profileDir);
  if (cached && cachedDir === profileDir && cachedSignature === signature) {
    return cached;
  }

  const base = readProfileJSON<Partial<CompanyConfig>>('companies.json');
  const suggested = loadSuggested(profileDir);
  const configuredTerms = Array.isArray(base.SEARCH_TERMS)
    ? base.SEARCH_TERMS
    : [];

  cachedDir = profileDir;
  cachedSignature = signature;
  cached = {
    ...base,
    SEARCH_TERMS: configuredTerms.length
      ? configuredTerms
      : deriveSearchTermsFromResume(profileDir),
    GREENHOUSE_COMPANIES: addNew(
      base.GREENHOUSE_COMPANIES || [],
      suggested.greenhouse
    ),
    ASHBY_COMPANIES: addNew(base.ASHBY_COMPANIES || [], suggested.ashby),
    LEVER_COMPANIES: addNew(base.LEVER_COMPANIES || [], suggested.lever),
    WORKABLE_COMPANIES: base.WORKABLE_COMPANIES || [],
    RIPPLING_COMPANIES: base.RIPPLING_COMPANIES || [],
    WORKDAY_COMPANIES: mergeWorkday(
      base.WORKDAY_COMPANIES || [],
      suggested.workday
    ),
  };
  return cached;
}
