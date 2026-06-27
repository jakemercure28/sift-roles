/**
 * Root + profile path resolution for the worker.
 *
 * The worker is self-contained TypeScript: validation, onboarding, ATS detection,
 * constants, and id derivation all live natively under ./lib. The only repo data
 * it reads at runtime is the active profile's companies.json (the company lists +
 * search terms the setup wizard writes), loaded as plain JSON via readProfileJSON.
 *
 * APP_ROOT defaults to the repo root (three levels up from src/lib/). In Docker
 * the repo's data.example is copied and APP_ROOT/DATA_DIR are set explicitly.
 *
 * DATA_DIR is the default (self-host) profile dir. On hosted Postgres the Go
 * backend serializes /scrape and passes the active tenant's profile dir per
 * request; setProfileDir overrides the active dir for that request so the
 * worker reads the right companies.json. Since /scrape is serialized, a single
 * module-level active dir is safe.
 */
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));

export const APP_ROOT = process.env.APP_ROOT
  ? resolve(process.env.APP_ROOT)
  : resolve(here, '..', '..', '..');

export const DATA_DIR = process.env.DATA_DIR
  ? resolve(process.env.DATA_DIR)
  : resolve(APP_ROOT, 'data');

let activeProfileDir = DATA_DIR;

/**
 * Override the active profile dir for the current request. An empty/undefined
 * dir resets to the default DATA_DIR (self-host, or the worker-only probe).
 */
export function setProfileDir(dir?: string): void {
  activeProfileDir = dir ? resolve(dir) : DATA_DIR;
}

/** The profile dir the worker currently reads from. */
export function getProfileDir(): string {
  return activeProfileDir;
}

/** Read + parse a JSON file from the active profile dir. */
export function readProfileJSON<T = unknown>(filename: string): T {
  return JSON.parse(
    readFileSync(resolve(getProfileDir(), filename), 'utf8')
  ) as T;
}
