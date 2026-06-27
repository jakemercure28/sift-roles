/**
 * Native TS port of scrapers/wwr.js. WeWorkRemotely publishes RSS feeds; items
 * are parsed by regex (CDATA or plain), the "Company: Job Title" subject is
 * split, and the description HTML is stripped. Verified byte-identical by
 * test/wwr.parity.test.ts.
 */
import { sleep, safeFetch } from '../lib/http.js';
import { stripHtml } from '../lib/html.js';
import { matchesSearchTerms } from '../lib/search.js';
import { MAX_DESCRIPTION_LENGTH, SCRAPER_DELAY_RSS_MS } from '../lib/config.js';
import { makeJobLead } from '../lib/joblead.js';
import type { JobLead } from '../types.js';

/**
 * Extract text content from an XML tag, handling both CDATA and plain text.
 * Returns empty string if tag not found.
 */
function extractTag(xml: string, tagName: string): string {
  const cdataRe = new RegExp(
    `<${tagName}><!\\[CDATA\\[([\\s\\S]*?)\\]\\]></${tagName}>`
  );
  const plainRe = new RegExp(`<${tagName}>([\\s\\S]*?)</${tagName}>`);
  const match = xml.match(cdataRe) || xml.match(plainRe);
  return match?.[1]?.trim() ?? '';
}

export async function scrapeWWR(): Promise<JobLead[]> {
  const jobs: JobLead[] = [];
  const feeds = [
    'https://weworkremotely.com/categories/remote-devops-sysadmin-jobs.rss',
    'https://weworkremotely.com/categories/remote-full-stack-programming-jobs.rss',
  ];

  for (const feedUrl of feeds) {
    const res = await safeFetch(feedUrl, {}, `wwr/${feedUrl}`);
    if (!res) {
      await sleep(SCRAPER_DELAY_RSS_MS);
      continue;
    }

    let xml: string;
    try {
      xml = await res.text();
    } catch {
      await sleep(SCRAPER_DELAY_RSS_MS);
      continue;
    }

    const items = xml.match(/<item>[\s\S]*?<\/item>/g) || [];
    for (const item of items) {
      const title = extractTag(item, 'title');
      const link = extractTag(item, 'link');
      const pubDate = extractTag(item, 'pubDate');
      const desc = extractTag(item, 'description');

      if (!title || !link) continue;

      // WWR title format: "Company Name: Job Title"
      const colonIdx = title.indexOf(':');
      const company = colonIdx > 0 ? title.slice(0, colonIdx).trim() : '';
      const jobTitle = colonIdx > 0 ? title.slice(colonIdx + 1).trim() : title;

      if (!matchesSearchTerms(jobTitle)) continue;

      const region = extractTag(item, 'region');
      jobs.push(
        makeJobLead({
          title: jobTitle,
          company,
          directApplyUrl: link,
          atsPlatformName: 'WeWorkRemotely',
          scrapedTimestamp: new Date().toISOString(),
          description: stripHtml(desc).slice(0, MAX_DESCRIPTION_LENGTH),
          location: region || 'Remote',
          postedAt: pubDate ? new Date(pubDate).toISOString().slice(0, 10) : '',
        })
      );
    }

    await sleep(SCRAPER_DELAY_RSS_MS);
  }

  return jobs;
}
