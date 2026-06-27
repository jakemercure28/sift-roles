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
import type { JobLead, IdentifiedLead } from './types.js';
import { APP_ROOT, getProfileDir } from './lib/interop.js';
import { deriveJobId } from './lib/joblead.js';
import { validateJobs } from './lib/validate.js';
import { isOnboarded } from './lib/onboarding.js';
import { scrapeGreenhouse } from './scrapers/greenhouse.js';
import { scrapeLever } from './scrapers/lever.js';
import { scrapeAshby } from './scrapers/ashby.js';
import { scrapeRemoteOK } from './scrapers/remoteok.js';
import { scrapeJobicy } from './scrapers/jobicy.js';
import { scrapeArbeitnow } from './scrapers/arbeitnow.js';
import { scrapeWellfound } from './scrapers/wellfound.js';
import { scrapeWWR } from './scrapers/wwr.js';
import { scrapeWorkable } from './scrapers/workable.js';
import { scrapeWorkday } from './scrapers/workday.js';
import { scrapeBuiltin } from './scrapers/builtin.js';
import { scrapeRippling } from './scrapers/rippling.js';

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

  const seen = new Set<string>();
  const jobs: IdentifiedLead[] = [];
  for (const result of settled) {
    if (result.status !== 'fulfilled') continue;
    for (const lead of result.value) {
      if (!lead.direct_apply_url) continue;
      const id = deriveJobId(lead);
      if (seen.has(id)) continue;
      seen.add(id);
      jobs.push({ ...lead, id });
    }
  }
  return jobs;
}
