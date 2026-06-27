/**
 * Regression coverage for the ported HTML/text normalization. stripHtml drives
 * fixMojibake + decodeEntities + tag strip + whitespace collapse. The expected
 * outputs below were frozen from lib/utils.js (the now-retired CJS original) when
 * the byte-for-byte port to src/lib/html.ts was verified, so this guards html.ts
 * against silent regressions without depending on the deleted JS module.
 */
import { test } from 'node:test';
import assert from 'node:assert/strict';

const CORPUS: ReadonlyArray<readonly [input: string, expected: string]> = [
  ['', ''],
  ['plain text', 'plain text'],
  ['<p>Hello <strong>world</strong></p>', 'Hello world'],
  ['A &amp; B &mdash; C &ndash; D &nbsp;end', 'A & B — C – D end'],
  ['numeric &#38; and hex &#x27; entities', "numeric & and hex ' entities"],
  ['double &amp;amp; collapse', 'double & collapse'],
  ['unicode escape \\u2014 in text', 'unicode escape — in text'],
  [
    'mojibake CafÃ© and grÃ¢ce Ã  the team',
    'mojibake CafÃ© and grÃ¢ce Ã the team',
  ],
  ['genuine accent grâce à café stays', 'genuine accent grâce à café stays'],
  [
    'collapse    multiple     spaces\n\n\tand   tabs',
    'collapse multiple spaces and tabs',
  ],
  ['<ul><li>one</li><li>two</li></ul> trailing   ', 'one two trailing'],
  ['&bull; &middot; &copy; &reg; &trade; &hellip;', '• · © ® ™ …'],
];

test('stripHtml matches the frozen lib/utils.js golden across the corpus', async () => {
  const { stripHtml } = await import('../src/lib/html.js');
  for (const [input, expected] of CORPUS) {
    assert.equal(
      stripHtml(input),
      expected,
      `stripHtml diverged for: ${JSON.stringify(input)}`
    );
  }
});

test('stripHtml honors the maxLen slice like the JS original', async () => {
  const { stripHtml } = await import('../src/lib/html.js');
  const long = 'word '.repeat(50);
  const expected: Record<number, string> = {
    0: '',
    5: 'word ',
    13: 'word word wor',
    80: 'word '.repeat(16),
  };
  for (const maxLen of [0, 5, 13, 80]) {
    assert.equal(stripHtml(long, maxLen), expected[maxLen]);
  }
});
