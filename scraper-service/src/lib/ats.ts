/**
 * ATS link detection, ported natively from the former root lib/atsDetector.js.
 * Aggregator scrapers use it to prefer a canonical ATS apply URL over the
 * aggregator's own link. Returns { platform, company } on a match, else null.
 */

export interface AtsMatch {
  platform: string;
  company: string | null;
}

const ATS_PATTERNS: { platform: string; re: RegExp }[] = [
  // https://jobs.ashbyhq.com/{company}/...
  { platform: 'Ashby', re: /^https?:\/\/jobs\.ashbyhq\.com\/([^/?#]+)/ },
  // https://boards.greenhouse.io/{company}/... or https://job-boards.greenhouse.io/{company}/...
  {
    platform: 'Greenhouse',
    re: /^https?:\/\/(?:boards|job-boards)\.greenhouse\.io\/([^/?#]+)/,
  },
  // https://jobs.lever.co/{company}/...
  { platform: 'Lever', re: /^https?:\/\/jobs\.lever\.co\/([^/?#]+)/ },
  // https://apply.workable.com/{company}/...
  { platform: 'Workable', re: /^https?:\/\/apply\.workable\.com\/([^/?#]+)/ },
  // https://company.wd5.myworkdayjobs.com/... or https://company.myworkdayjobs.com/...
  {
    platform: 'Workday',
    re: /^https?:\/\/([^/.]+)\.(?:wd\d+\.|)myworkdayjobs\.com\//,
  },
  // https://ats.rippling.com/{company}/jobs/...
  { platform: 'Rippling', re: /^https?:\/\/ats\.rippling\.com\/([^/?#]+)\// },
];

export function detectAts(url: string): AtsMatch | null {
  if (!url) return null;
  if (/[?&]gh_jid=\d+/.test(url)) {
    return { platform: 'Greenhouse', company: null };
  }
  for (const { platform, re } of ATS_PATTERNS) {
    const m = url.match(re);
    if (m) return { platform, company: m[1] ?? null };
  }
  return null;
}
