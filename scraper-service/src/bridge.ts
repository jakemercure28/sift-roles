/**
 * Scraper registry + orchestration for the worker.
 *
 * The strangler migration is complete: all 12 scrapers are native TypeScript
 * under ./scrapers/*.ts (each verified byte-identical to its former CommonJS
 * original by an offline parity test). The Node interop that used to execute the
 * JS scrapers in <APP_ROOT>/scrapers has been removed.
 *
 * Id derivation (deriveJobId), contract validation (validateJobs), and the
 * onboarding gate (isOnboarded) are all native TS now (./lib/*); the CommonJS
 * contract bridge has been removed. They still apply to every scraper, so the
 * emitted JobLead/id is unchanged from the bridged era.
 */
import type {
  JobLead,
  IdentifiedLead,
  BoardRef,
  BoardFetchResult,
} from './types.js';
import { APP_ROOT, getProfileDir } from './lib/interop.js';
import { HostPacer } from './lib/http.js';
import { scrapeBoardsConditional } from './lib/boardfetch.js';
import type { BoardScrapeOutcome } from './lib/boardfetch.js';
import { deriveJobId } from './lib/joblead.js';
import { validateJobs } from './lib/validate.js';
import { isOnboarded } from './lib/onboarding.js';
import { scrapeGreenhouse, greenhouseOptions } from './scrapers/greenhouse.js';
import { scrapeLever, leverOptions } from './scrapers/lever.js';
import { scrapeAshby, ashbyOptions } from './scrapers/ashby.js';
import { scrapeRemoteOK } from './scrapers/remoteok.js';
import { scrapeJobicy } from './scrapers/jobicy.js';
import { scrapeArbeitnow } from './scrapers/arbeitnow.js';
import { scrapeWellfound } from './scrapers/wellfound.js';
import { scrapeWWR } from './scrapers/wwr.js';
import { scrapeWorkable, scrapeWorkableBoards } from './scrapers/workable.js';
import { scrapeWorkday, scrapeWorkdayBoards } from './scrapers/workday.js';
import type { WorkdayCompany } from './scrapers/workday.js';
import { scrapeBuiltin } from './scrapers/builtin.js';
import { scrapeRippling } from './scrapers/rippling.js';
import { scrapeGem, scrapeGemBoards } from './scrapers/gem.js';
import {
  scrapeSmartRecruiters,
  scrapeSmartRecruitersBoards,
} from './scrapers/smartrecruiters.js';

type ScrapeFn = () => Promise<JobLead[]>;

/** All native TS scrapers, keyed by platform label. */
const SCRAPERS: Record<string, ScrapeFn> = {
  greenhouse: scrapeGreenhouse,
  lever: scrapeLever,
  ashby: scrapeAshby,
  remoteok: scrapeRemoteOK,
  jobicy: scrapeJobicy,
  arbeitnow: scrapeArbeitnow,
  wellfound: scrapeWellfound,
  wwr: scrapeWWR,
  workable: scrapeWorkable,
  workday: scrapeWorkday,
  builtin: scrapeBuiltin,
  rippling: scrapeRippling,
  gem: scrapeGem,
  smartrecruiters: scrapeSmartRecruiters,
};

// Canonical platform order, preserved so the cross-platform dedupe in runScrape
// attributes a shared job to the same platform run-to-run.
const PLATFORM_ORDER = [
  'greenhouse',
  'lever',
  'workable',
  'wellfound',
  'remoteok',
  'jobicy',
  'arbeitnow',
  'wwr',
  'ashby',
  'workday',
  'builtin',
  'rippling',
  'gem',
  'smartrecruiters',
] as const;

function loadScrapeFn(platform: string): ScrapeFn {
  const fn = SCRAPERS[platform];
  if (!fn) throw new Error(`Unknown platform: ${platform}`);
  return fn;
}

// Contract validation and the onboarding gate are native TS (./lib/validate.js,
// ./lib/onboarding.js), imported above. The onboarding gate matters because the
// Go scrape path must refuse to run on an un-set-up profile or it would scrape
// data.example demo defaults on a fresh install.

/** True when the active profile is ready for automated scraping. Never throws. */
export function onboarded(): boolean {
  try {
    return isOnboarded({ profileDir: getProfileDir(), repoRoot: APP_ROOT });
  } catch {
    return false;
  }
}

export function listPlatforms(): string[] {
  return PLATFORM_ORDER.filter((p) => p in SCRAPERS);
}

/** Run a single platform, validate against the JobLead contract. Never throws. */
export async function runPlatform(platform: string): Promise<JobLead[]> {
  try {
    const raw = await loadScrapeFn(platform)();
    return validateJobs(Array.isArray(raw) ? raw : [], platform);
  } catch (err) {
    process.stderr.write(
      JSON.stringify({
        level: 'error',
        platform,
        error: err instanceof Error ? err.message : String(err),
      }) + '\n'
    );
    return [];
  }
}

/**
 * Run the requested platforms (all if none given) in parallel, then dedupe
 * across platforms by derived job id (matching scraper.js). Leads without a
 * direct_apply_url are dropped. Each returned lead carries its canonical `id`
 * (from deriveJobId) so the Go writer can insert without reimplementing it.
 */
export async function runScrape(
  requested?: string[]
): Promise<IdentifiedLead[]> {
  if (!onboarded()) {
    process.stderr.write(
      JSON.stringify({
        level: 'warn',
        msg: 'scrape skipped: profile not onboarded',
        dataDir: getProfileDir(),
      }) + '\n'
    );
    return [];
  }

  const known = listPlatforms();
  const platforms =
    requested && requested.length > 0
      ? requested.filter((p) => known.includes(p))
      : known;

  const settled = await Promise.allSettled(platforms.map(runPlatform));

  const results: JobLead[][] = [];
  for (const result of settled) {
    if (result.status === 'fulfilled') results.push(result.value);
  }
  return identifyAndDedupe(results);
}

/** Cross-platform id derivation + dedupe shared by both scrape modes. */
function identifyAndDedupe(results: JobLead[][]): IdentifiedLead[] {
  const seen = new Set<string>();
  const jobs: IdentifiedLead[] = [];
  for (const leads of results) {
    for (const lead of leads) {
      if (!lead.direct_apply_url) continue;
      const id = deriveJobId(lead);
      if (seen.has(id)) continue;
      seen.add(id);
      jobs.push({ ...lead, id });
    }
  }
  return jobs;
}

// Board mode spaces request starts to the same host: the cross-tenant board
// union can put thousands of boards on one ATS API host in a single pass.
const BOARD_HOST_MIN_INTERVAL_MS = 150;

/** Platforms board mode can scrape (the registry's verified board platforms). */
export const BOARD_PLATFORMS = [
  'greenhouse',
  'lever',
  'ashby',
  'workable',
  'workday',
  'gem',
  'smartrecruiters',
] as const;

/** What one board-mode scrape produced. */
export interface BoardScrapeReport {
  jobs: IdentifiedLead[];
  // Per-board fetch outcomes for the single-request platforms, so the caller
  // can persist etag/content-hash state and skip unchanged boards next cycle.
  boards: BoardFetchResult[];
}

/**
 * Board mode: scrape exactly the given boards, returning ALL live postings
 * with no search-term filtering (term matching happens Go-side when the
 * harvest is distributed to tenants). Reads no profile files, so the
 * onboarding gate does not apply. Validation, id derivation, and
 * cross-platform dedupe are identical to runScrape, so a lead scraped in
 * either mode carries the same id. Single-request boards (greenhouse, lever,
 * ashby) fetch conditionally against the etag/contentHash carried on the
 * BoardRef and are skipped when unchanged.
 */
export async function runBoardScrape(
  boards: BoardRef[]
): Promise<BoardScrapeReport> {
  const pacer = new HostPacer(BOARD_HOST_MIN_INTERVAL_MS);

  const refs = (platform: string): BoardRef[] => {
    const seen = new Set<string>();
    return boards.filter((b) => {
      if (b.platform !== platform || !b.slug || seen.has(b.slug)) return false;
      seen.add(b.slug);
      return true;
    });
  };

  const workdayEntries: WorkdayCompany[] = [];
  const seenWorkday = new Set<string>();
  for (const b of boards) {
    if (b.platform !== 'workday' || !b.slug || !b.wd || !b.board) continue;
    const key = `${b.slug}/${b.board}`;
    if (seenWorkday.has(key)) continue;
    seenWorkday.add(key);
    workdayEntries.push({
      sub: b.slug,
      wd: b.wd,
      board: b.board,
      label: b.label || '',
    });
  }

  const runs: Promise<BoardScrapeOutcome>[] = [];
  const asOutcome = (p: Promise<JobLead[]>): Promise<BoardScrapeOutcome> =>
    p.then((leads) => ({ leads, results: [] }));

  const greenhouse = refs('greenhouse');
  if (greenhouse.length)
    runs.push(
      scrapeBoardsConditional(greenhouseOptions([]), greenhouse, pacer)
    );
  const lever = refs('lever');
  if (lever.length)
    runs.push(scrapeBoardsConditional(leverOptions([]), lever, pacer));
  const ashby = refs('ashby');
  if (ashby.length)
    runs.push(scrapeBoardsConditional(ashbyOptions([]), ashby, pacer));
  const workable = refs('workable');
  if (workable.length)
    runs.push(asOutcome(scrapeWorkableBoards(workable.map((b) => b.slug))));
  if (workdayEntries.length) runs.push(scrapeWorkdayBoards(workdayEntries));
  const gem = refs('gem');
  if (gem.length) runs.push(scrapeGemBoards(gem.map((b) => b.slug)));
  const smartrecruiters = refs('smartrecruiters');
  if (smartrecruiters.length)
    runs.push(scrapeSmartRecruitersBoards(smartrecruiters.map((b) => b.slug)));

  const settled = await Promise.allSettled(runs);
  const leadSets: JobLead[][] = [];
  const boardResults: BoardFetchResult[] = [];
  for (const result of settled) {
    if (result.status !== 'fulfilled') continue;
    leadSets.push(validateJobs(result.value.leads, 'board'));
    boardResults.push(...result.value.results);
  }
  return { jobs: identifyAndDedupe(leadSets), boards: boardResults };
}
