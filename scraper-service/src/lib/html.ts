/**
 * HTML/text normalization, ported verbatim from lib/utils.js so native scrapers
 * produce byte-identical descriptions. The order matters: fixMojibake ->
 * decodeEntities -> tag strip -> whitespace collapse -> trim -> slice. Any drift
 * here changes description bytes and is caught by test/html.test.ts (diffed
 * against the JS original).
 */
import { MAX_DESCRIPTION_LENGTH } from './config.js';

const HTML_ENTITY_MAP: Record<string, string> = {
  amp: '&',
  lt: '<',
  gt: '>',
  quot: '"',
  apos: "'",
  nbsp: ' ',
  mdash: '—',
  ndash: '–',
  ldquo: '“',
  rdquo: '”',
  lsquo: '‘',
  rsquo: '’',
  hellip: '…',
  bull: '•',
  middot: '·',
  copy: '©',
  reg: '®',
  trade: '™',
};

// Windows-1252 codepoints for bytes 0x80-0x9F (the range where cp1252 diverges
// from Latin-1). Used to reverse "mojibake" — UTF-8 text that was decoded as
// cp1252/Latin-1 upstream, e.g. an en-dash showing up as "â€“".
const CP1252_HIGH: Record<number, number> = {
  0x80: 0x20ac,
  0x82: 0x201a,
  0x83: 0x0192,
  0x84: 0x201e,
  0x85: 0x2026,
  0x86: 0x2020,
  0x87: 0x2021,
  0x88: 0x02c6,
  0x89: 0x2030,
  0x8a: 0x0160,
  0x8b: 0x2039,
  0x8c: 0x0152,
  0x8e: 0x017d,
  0x91: 0x2018,
  0x92: 0x2019,
  0x93: 0x201c,
  0x94: 0x201d,
  0x95: 0x2022,
  0x96: 0x2013,
  0x97: 0x2014,
  0x98: 0x02dc,
  0x99: 0x2122,
  0x9a: 0x0161,
  0x9b: 0x203a,
  0x9c: 0x0153,
  0x9e: 0x017e,
  0x9f: 0x0178,
};

const UNICODE_TO_CP1252: Record<number, number> = {};
for (const [byte, cp] of Object.entries(CP1252_HIGH))
  UNICODE_TO_CP1252[cp] = Number(byte);

// Repair text that was UTF-8 but got decoded as cp1252/Latin-1 before we received
// it (common from some job-board APIs). We re-encode the string back to the bytes
// that reading would have produced, then decode as UTF-8. A real multibyte char we
// can't map, or a round-trip that yields the replacement char, means the input
// wasn't mojibake (e.g. legitimate French "grâce à"), so we return it untouched.
export function fixMojibake(s: string): string {
  const str = String(s || '');
  // Fast path: mojibake always contains a cp1252/Latin-1 lead byte rendered as
  // À-ß (U+00C0-U+00DF) or â (U+00E2). Clean ASCII / most accented text skips this.
  if (!/[À-ßâ]/.test(str)) return str;
  const bytes: number[] = [];
  for (const ch of str) {
    const cp = ch.codePointAt(0) as number;
    if (cp <= 0xff) bytes.push(cp);
    else if (UNICODE_TO_CP1252[cp] != null)
      bytes.push(UNICODE_TO_CP1252[cp] as number);
    else return str; // genuine non-cp1252 multibyte char: not repairable mojibake
  }
  const decoded = Buffer.from(bytes).toString('utf8');
  if (decoded.includes('�')) return str; // invalid UTF-8 -> wasn't mojibake
  return decoded;
}

export function decodeEntities(s: string): string {
  let decoded = String(s || '');

  // Run a few passes so inputs like "&amp;amp;" fully collapse without risking an infinite loop.
  for (let i = 0; i < 3; i += 1) {
    const next = decoded
      .replace(/\\u([0-9a-f]{4})/gi, (_, h: string) =>
        String.fromCodePoint(parseInt(h, 16))
      )
      .replace(
        /&([a-z]+);/gi,
        (match: string, entity: string) =>
          HTML_ENTITY_MAP[entity.toLowerCase()] || match
      )
      .replace(/&#(\d+);/g, (_, n: string) => String.fromCodePoint(Number(n)))
      .replace(/&#x([0-9a-f]+);/gi, (_, h: string) =>
        String.fromCodePoint(parseInt(h, 16))
      );

    if (next === decoded) break;
    decoded = next;
  }

  return decoded;
}

export function stripHtml(
  text: string,
  maxLen = MAX_DESCRIPTION_LENGTH
): string {
  const decoded = decodeEntities(fixMojibake(text || ''));
  return decoded
    .replace(/<[^>]+>/g, ' ')
    .replace(/\s{2,}/g, ' ')
    .trim()
    .slice(0, maxLen);
}
