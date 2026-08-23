package discovery

// Common Crawl registry seeding hunts public ATS board slugs from the Common
// Crawl URL index (a license-permissive primary source), verifies each board is
// live, and returns the boards for the caller to upsert into the shared registry.
// It grows the global company pool with employers no tenant has encountered yet,
// at zero Gemini cost. It complements the URL harvest (which mines boards the
// scraper already reached) by reaching boards it has not: the board slug sits in
// the crawled apply URL, so the same ParseATSBoardURL/VerifyBoards path applies.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	ccCollInfoURL  = "https://index.commoncrawl.org/collinfo.json"
	ccDefaultLimit = 150000
	ccFetchTimeout = 5 * time.Minute
	ccVerifyConc   = 25
	ccLineCap      = 1 << 20 // 1 MiB max CDX line
)

// ccQuery is one CDX query. matchDomain=true matches a registrable domain and all
// its subdomains (used for Workday, whose board tenant is the subdomain); false
// treats pattern as a host prefix with a trailing /* wildcard.
type ccQuery struct {
	pattern     string
	matchDomain bool
}

// ccQueries covers the public-API ATS platforms this pipeline can scrape from a
// host-prefixed URL. Lever is included though recent crawls barely cover
// jobs.lever.co; it costs one cheap query and self-heals if coverage returns.
// SmartRecruiters is well covered (both jobs. and careers. hosts).
var ccQueries = []ccQuery{
	{"job-boards.greenhouse.io/*", false},
	{"boards.greenhouse.io/*", false},
	{"jobs.ashbyhq.com/*", false},
	{"jobs.lever.co/*", false},
	{"jobs.smartrecruiters.com/*", false},
	{"careers.smartrecruiters.com/*", false},
	{"myworkdayjobs.com", true},
}

// CommonCrawlConfig controls a seed run. Zero values fall back to defaults.
type CommonCrawlConfig struct {
	HTTPClient  *http.Client
	Limit       int    // max CDX rows per query
	Concurrency int    // board verification concurrency
	Crawl       string // explicit crawl id override (empty -> newest N)
	Crawls      int    // number of most-recent monthly crawls to sweep (0 -> 1)
	Log         *slog.Logger
}

// CommonCrawlReport summarizes a seed run.
type CommonCrawlReport struct {
	Crawls     []string // crawl ids swept, newest first
	Candidates int
	Live       []VerifiedBoard
}

// HarvestCommonCrawl extracts, dedups, and liveness-verifies ATS boards from the
// latest (or configured) Common Crawl index. It returns the live boards; the
// caller upserts them into the registry. A failed CDX query is logged and
// skipped, not fatal, so one platform outage never aborts the whole run.
func HarvestCommonCrawl(ctx context.Context, cfg CommonCrawlConfig) (CommonCrawlReport, error) {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: ccFetchTimeout}
	}
	limit := cfg.Limit
	if limit <= 0 {
		limit = ccDefaultLimit
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	var crawls []string
	if cfg.Crawl != "" {
		crawls = []string{cfg.Crawl}
	} else {
		n := cfg.Crawls
		if n <= 0 {
			n = 1
		}
		ids, err := recentCrawlIDs(ctx, client, n)
		if err != nil {
			return CommonCrawlReport{}, err
		}
		crawls = ids
	}

	// Sweep every crawl into one dedup set: a board seen in any crawl is a
	// candidate, and re-seeing it across crawls is free (the map dedups).
	seen := map[string]VerifiedBoard{}
	for _, crawl := range crawls {
		for _, q := range ccQueries {
			urls, err := ccFetchURLs(ctx, client, crawl, q, limit)
			if err != nil {
				if ctx.Err() != nil {
					return CommonCrawlReport{}, ctx.Err()
				}
				log.Warn("common crawl query failed", "crawl", crawl, "pattern", q.pattern, "error", err)
				continue
			}
			for _, u := range urls {
				b, ok := ParseATSBoardURL(u)
				if !ok {
					continue
				}
				if k := BoardKey(b); k != "" {
					if _, dup := seen[k]; !dup {
						seen[k] = b
					}
				}
			}
		}
		log.Info("common crawl swept", "crawl", crawl, "distinct_boards_so_far", len(seen))
	}

	cands := make([]VerifiedBoard, 0, len(seen))
	for _, b := range seen {
		cands = append(cands, b)
	}
	conc := cfg.Concurrency
	if conc <= 0 {
		conc = ccVerifyConc
	}
	live := VerifyBoards(ctx, client, cands, conc)
	log.Info("common crawl harvest complete", "crawls", crawls, "candidates", len(cands), "live", len(live))
	return CommonCrawlReport{Crawls: crawls, Candidates: len(cands), Live: live}, nil
}

// recentCrawlIDs returns the ids of the n most recent Common Crawl monthly
// indexes (newest first).
func recentCrawlIDs(ctx context.Context, client *http.Client, n int) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ccCollInfoURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("collinfo status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	ids, err := decodeCrawlIDs(body)
	if err != nil {
		return nil, err
	}
	if n > 0 && n < len(ids) {
		ids = ids[:n]
	}
	return ids, nil
}

// decodeCrawlIDs parses a collinfo.json body into crawl ids, newest first.
func decodeCrawlIDs(body []byte) ([]string, error) {
	var colls []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &colls); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(colls))
	for _, c := range colls {
		if c.ID != "" {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no common crawl collections listed")
	}
	return ids, nil
}

// ccFetchURLs streams one CDX query against the latest crawl index and returns
// the captured URLs.
func ccFetchURLs(ctx context.Context, client *http.Client, crawl string, q ccQuery, limit int) ([]string, error) {
	base := fmt.Sprintf("https://index.commoncrawl.org/%s-index", url.QueryEscape(crawl))
	return ccFetchURLsFrom(ctx, client, base, q, limit)
}

// ccFetchURLsFrom streams one CDX query against an explicit index base URL (a
// seam for tests). A 404 means "no captures for this pattern", an empty result
// rather than an error.
func ccFetchURLsFrom(ctx context.Context, client *http.Client, base string, q ccQuery, limit int) ([]string, error) {
	endpoint := fmt.Sprintf("%s?url=%s&output=json&fl=url&limit=%d",
		base, url.QueryEscape(q.pattern), limit)
	if q.matchDomain {
		endpoint += "&matchType=domain"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cdx status %d", resp.StatusCode)
	}
	var out []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), ccLineCap)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil || row.URL == "" {
			continue
		}
		out = append(out, row.URL)
	}
	return out, sc.Err()
}

// BoardKey is the platform-qualified dedup key for a board: Workday keys on its
// full tenant/board path, API boards on the collapsed slug.
func BoardKey(b VerifiedBoard) string {
	platform := strings.ToLower(strings.TrimSpace(b.Platform))
	if platform == "workday" {
		if b.Workday == nil {
			return ""
		}
		return "workday:" + WorkdayKey(*b.Workday)
	}
	if b.Slug == "" {
		return ""
	}
	if platform == "smartrecruiters" {
		// SmartRecruiters identifiers are case-SENSITIVE, so keep case variants
		// (1Huddle vs 1HUDDLE) distinct in the dedup set. Collapsing them onto one
		// lowercase key could drop the correct-case slug and keep a wrong-case
		// twin that then fails liveness verification, losing the board entirely.
		return "smartrecruiters:" + b.Slug
	}
	return platform + ":" + SlugKey(b.Slug)
}
