import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const TERM_LIMIT = 8;

const RESUME_TERM_LEXICON = [
  'software engineer',
  'devops',
  'sre',
  'platform engineer',
  'data engineer',
  'data analyst',
  'product manager',
  'project manager',
  'program manager',
  'marketing',
  'sales',
  'customer success',
  'operations',
  'recruiter',
  'human resources',
  'accountant',
  'financial analyst',
  'nurse',
  'teacher',
  'merchandising',
  'merchandiser',
  'buyer',
  'planner',
  'retail',
  'store manager',
  'visual merchandising',
];

const ROLE_LINE_RE =
  /\b(engineer|developer|manager|director|analyst|associate|coordinator|specialist|designer|architect|consultant|administrator|operator|technician|nurse|teacher|accountant|recruiter|merchandis(?:er|ing)|buyer|planner|retail|sales|marketing|operations)\b/i;

export function normalizeSearchTerm(term: string): string {
  return term
    .toLowerCase()
    .replace(/\([^)]*\)/g, '')
    .replace(/[^a-z0-9+.#/ -]+/g, ' ')
    .replace(/\s+/g, ' ')
    .trim();
}

export function deriveSearchTermsFromResumeText(resume: string): string[] {
  const text = resume.trim();
  if (!text) return [];

  const terms: string[] = [];
  const seen = new Set<string>();
  const add = (raw: string) => {
    const term = normalizeSearchTerm(raw);
    if (term.length < 3 || seen.has(term)) return;
    seen.add(term);
    terms.push(term);
  };

  for (const rawLine of text.split(/\r?\n/)) {
    let line = rawLine.trim().replace(/^[-* ]+/, '');
    if (!line || line.length > 90 || line.endsWith(':') || line.endsWith('.'))
      continue;
    if (!ROLE_LINE_RE.test(line)) continue;

    const lower = line.toLowerCase();
    for (const sep of [' | ', ' - ', ' -- ', ' at ', ' @ ', ',']) {
      const idx = lower.indexOf(sep);
      if (idx > 0) {
        line = line.slice(0, idx);
        break;
      }
    }
    add(line);
    if (terms.length >= TERM_LIMIT) return terms;
  }

  const lowerText = text.toLowerCase();
  for (const term of RESUME_TERM_LEXICON) {
    if (lowerText.includes(term)) add(term);
    if (terms.length >= TERM_LIMIT) break;
  }
  return terms;
}

export function deriveSearchTermsFromResume(profileDir: string): string[] {
  try {
    return deriveSearchTermsFromResumeText(
      readFileSync(resolve(profileDir, 'resume.md'), 'utf8')
    );
  } catch {
    return [];
  }
}
