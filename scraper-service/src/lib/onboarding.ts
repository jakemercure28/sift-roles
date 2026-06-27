/**
 * Onboarding gate, ported natively from the former root lib/onboarding.js.
 *
 * A fresh install copies data.example into data/, which ships demo company lists
 * and search terms. The scrape path must refuse to run until the user finishes
 * the setup wizard, or it would scrape those demo defaults. "Onboarded" means the
 * wizard wrote the marker, or a pre-marker install already looks real (key set +
 * non-empty resume + a companies.json that differs from the shipped example).
 */
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

export const MARKER_NAME = '.onboarded';

export function markerPath(profileDir: string): string {
  return join(profileDir, MARKER_NAME);
}

export function markOnboarded(profileDir: string): boolean {
  try {
    writeFileSync(
      markerPath(profileDir),
      new Date().toISOString() + '\n',
      'utf8'
    );
    return true;
  } catch {
    return false;
  }
}

function readFileOrEmpty(filePath: string): string {
  try {
    return readFileSync(filePath, 'utf8');
  } catch {
    return '';
  }
}

// True when discovery has appended at least one verified board to the profile's
// suggested-companies.json. The scrape config (config.ts companyConfig) merges
// those boards in and scrapes them, so a profile with discovered boards is
// genuinely scrapeable even when companies.json is still the empty example
// (e.g. a tenant whose companies came entirely from resume-driven discovery and
// who set no manual companies or search terms). Without this, such a tenant
// passed the dashboard's onboarded check and had real boards to scrape, yet the
// scrape gate skipped it forever because it only inspected companies.json.
function hasDiscoveredBoards(profileDir: string): boolean {
  const raw = readFileOrEmpty(join(profileDir, 'suggested-companies.json'));
  if (!raw) return false;
  try {
    const data = JSON.parse(raw) as Record<string, unknown>;
    return ['greenhouse', 'ashby', 'lever', 'workday'].some(
      (k) => Array.isArray(data[k]) && (data[k] as unknown[]).length > 0
    );
  } catch {
    return false;
  }
}

// Whitespace-insensitive compare so trivial formatting differences don't read as
// "the user customized it".
function sameAsExampleCompanies(profileDir: string, repoRoot: string): boolean {
  const profileCompanies = readFileOrEmpty(join(profileDir, 'companies.json'));
  if (!profileCompanies) return false;
  const exampleCompanies = readFileOrEmpty(
    join(repoRoot, 'data.example', 'companies.json')
  );
  if (!exampleCompanies) return false;
  const normalize = (s: string) => s.replace(/\s+/g, ' ').trim();
  return normalize(profileCompanies) === normalize(exampleCompanies);
}

export interface OnboardedOptions {
  profileDir: string;
  repoRoot: string;
  env?: NodeJS.ProcessEnv;
}

/**
 * Returns true when the profile is ready for automated scraping. Marker wins;
 * otherwise auto-heal pre-marker installs that already look real, writing the
 * marker so the check is cheap next time.
 */
export function isOnboarded({
  profileDir,
  repoRoot,
  env = process.env,
}: OnboardedOptions): boolean {
  if (existsSync(markerPath(profileDir))) return true;

  const hasKey = Boolean((env.GEMINI_API_KEY || '').trim());
  const hasResume =
    readFileOrEmpty(join(profileDir, 'resume.md')).trim().length > 0;
  // Scrapeable when the user customized companies.json OR discovery already found
  // boards (suggested-companies.json) the scraper will merge in and scrape.
  const hasScrapeableCompanies =
    !sameAsExampleCompanies(profileDir, repoRoot) ||
    hasDiscoveredBoards(profileDir);

  if (hasKey && hasResume && hasScrapeableCompanies) {
    markOnboarded(profileDir);
    return true;
  }
  return false;
}
