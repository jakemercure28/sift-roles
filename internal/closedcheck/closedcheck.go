// Package closedcheck probes canonical ATS postings and closes rows that have
// disappeared from their source board.
package closedcheck

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"job-search-automation/internal/db"
)

const (
	defaultConcurrency = 10
	defaultDelay       = 300 * time.Millisecond
	requestTimeout     = 10 * time.Second
)

var closedBody = regexp.MustCompile(`(?i)no longer (accepting|open|available)|job (has been|is) (closed|removed|filled)`)
var ashbyUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Eligibility is a native ATS row that can be auto-closed safely.
type Eligibility struct {
	Platform string
	Slug     string
	JobID    string
}

// HTTPResult is the only response data the checker needs.
type HTTPResult struct {
	StatusCode int
	Body       []byte
}

// Fetcher performs an HTTP GET. It returns nil for network failures so callers
// skip instead of closing on uncertainty.
type Fetcher func(ctx context.Context, rawURL string) (*HTTPResult, error)

// Cache carries probe state across Run calls within one maintenance sweep. The
// sweep fans out per tenant, and tenants routinely hold the same postings, so
// sharing one Cache across the sweep means each distinct posting is probed once
// and each Ashby board is fetched once, no matter how many tenants hold them.
// Safe for concurrent use.
type Cache struct {
	mu       sync.Mutex
	verdicts map[string]string          // platform|slug|jobID -> "open"|"closed"
	ashby    map[string]map[string]bool // board slug -> live posting ids
	slugMu   map[string]*sync.Mutex     // per-slug lock: one board fetch in flight
}

// NewCache returns an empty probe cache for one maintenance sweep.
func NewCache() *Cache {
	return &Cache{
		verdicts: map[string]string{},
		ashby:    map[string]map[string]bool{},
		slugMu:   map[string]*sync.Mutex{},
	}
}

func probeKey(e Eligibility) string { return e.Platform + "|" + e.Slug + "|" + e.JobID }

func (c *Cache) verdict(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.verdicts[key]
	return v, ok
}

// putVerdict caches a definite probe outcome. Skips ("") and errors are never
// cached: they may be transient, and a later tenant's probe should retry.
func (c *Cache) putVerdict(key, status string) {
	if status == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.verdicts[key] = status
}

func (c *Cache) ashbyBoard(slug string) (map[string]bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	jobs, ok := c.ashby[slug]
	return jobs, ok
}

func (c *Cache) putAshbyBoard(slug string, jobs map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ashby[slug] = jobs
}

// slugLock returns the per-slug mutex, creating it on first use. Concurrent
// checks of the same Ashby board serialize on it so the board is fetched once;
// checks of different boards (and all Greenhouse probes) stay parallel.
func (c *Cache) slugLock(slug string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.slugMu[slug]
	if !ok {
		m = &sync.Mutex{}
		c.slugMu[slug] = m
	}
	return m
}

// Config controls one check pass.
type Config struct {
	Concurrency int
	Delay       time.Duration
	Fetch       Fetcher
	Log         *slog.Logger
	// Cache, when set, shares probe verdicts and Ashby board snapshots with
	// other Run calls in the same maintenance sweep so cross-tenant duplicate
	// postings are probed once. Nil gets a fresh per-run cache.
	Cache *Cache
}

// Result reports one check pass.
type Result struct {
	Checked int
	Closed  int
	Skipped int
	Errored int
}

// AutoCloseEligibility ports getAutoCloseEligibility from scripts/check-closed.js.
func AutoCloseEligibility(job db.CloseCheckJob) *Eligibility {
	if job.URL == "" || job.ID == "" {
		return nil
	}
	u, err := url.Parse(job.URL)
	if err != nil {
		return nil
	}
	platform := strings.ToLower(job.Platform)
	parts := pathParts(u.Path)

	if platform == "greenhouse" {
		if !strings.HasPrefix(job.ID, "greenhouse-") || u.Scheme != "https" {
			return nil
		}
		if u.Hostname() != "boards.greenhouse.io" && u.Hostname() != "job-boards.greenhouse.io" {
			return nil
		}
		if len(parts) != 3 || parts[1] != "jobs" {
			return nil
		}
		jobID := parts[2]
		if jobID != strings.TrimPrefix(job.ID, "greenhouse-") {
			return nil
		}
		return &Eligibility{Platform: "Greenhouse", Slug: parts[0], JobID: jobID}
	}

	if platform == "ashby" {
		if !strings.HasPrefix(job.ID, "ashby-") || u.Scheme != "https" || u.Hostname() != "jobs.ashbyhq.com" {
			return nil
		}
		if len(parts) != 2 {
			return nil
		}
		slug, uuid := parts[0], parts[1]
		if !ashbyUUID.MatchString(uuid) || uuid != strings.TrimPrefix(job.ID, "ashby-") {
			return nil
		}
		return &Eligibility{Platform: "Ashby", Slug: slug, JobID: uuid}
	}

	// Gem has no id-prefix scheme (see joblead.ts jobIdFor), so unlike
	// Greenhouse/Ashby above there is no job.ID invariant to cross-check; the
	// platform label plus the exact jobs.gem.com host is narrow enough on its
	// own (Gem does not support custom-domain boards).
	if platform == "gem" {
		if u.Scheme != "https" || u.Hostname() != "jobs.gem.com" {
			return nil
		}
		if len(parts) != 2 {
			return nil
		}
		return &Eligibility{Platform: "Gem", Slug: parts[0], JobID: parts[1]}
	}
	return nil
}

func pathParts(path string) []string {
	raw := strings.Split(path, "/")
	var parts []string
	for _, p := range raw {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

func defaultFetcher(client *http.Client) Fetcher {
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	return func(ctx context.Context, rawURL string) (*HTTPResult, error) {
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, nil
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; job-search-bot/1.0)")
		res, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, nil
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return &HTTPResult{StatusCode: res.StatusCode, Body: body}, nil
	}
}

// CheckJob returns "open", "closed", or "" when the row should be skipped.
// Definite verdicts are cached in cache so the same posting held by another
// tenant (or another row) is not re-probed within the sweep.
func CheckJob(ctx context.Context, fetch Fetcher, job db.CloseCheckJob, cache *Cache) (string, error) {
	eligibility := AutoCloseEligibility(job)
	if eligibility == nil {
		return "", nil
	}
	key := probeKey(*eligibility)
	if status, ok := cache.verdict(key); ok {
		return status, nil
	}
	var status string
	var err error
	switch eligibility.Platform {
	case "Greenhouse":
		status, err = checkGreenhouse(ctx, fetch, *eligibility)
	case "Ashby":
		status, err = checkAshby(ctx, fetch, *eligibility, cache)
	case "Gem":
		status, err = checkGem(ctx, fetch, *eligibility)
	}
	if err == nil {
		cache.putVerdict(key, status)
	}
	return status, err
}

func checkGreenhouse(ctx context.Context, fetch Fetcher, e Eligibility) (string, error) {
	apiURL := "https://api.greenhouse.io/v1/boards/" + e.Slug + "/jobs/" + e.JobID
	res, err := fetch(ctx, apiURL)
	if err != nil || res == nil {
		return "", err
	}
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusGone {
		return "closed", nil
	}
	if res.StatusCode >= 400 && res.StatusCode < 500 && res.StatusCode != http.StatusTooManyRequests {
		if closedBody.Match(res.Body) {
			return "closed", nil
		}
	}
	return "open", nil
}

func checkAshby(ctx context.Context, fetch Fetcher, e Eligibility, cache *Cache) (string, error) {
	// One board fetch per slug: the per-slug lock makes concurrent checks of the
	// same board wait for the first fetch instead of duplicating it, while other
	// boards proceed in parallel.
	lock := cache.slugLock(e.Slug)
	lock.Lock()
	jobs, ok := cache.ashbyBoard(e.Slug)
	if !ok {
		res, err := fetch(ctx, "https://api.ashbyhq.com/posting-api/job-board/"+e.Slug)
		if err != nil || res == nil || res.StatusCode < 200 || res.StatusCode >= 300 {
			lock.Unlock()
			return "", err
		}
		var board struct {
			Jobs []struct {
				ID string `json:"id"`
			} `json:"jobs"`
		}
		if err := json.Unmarshal(res.Body, &board); err != nil {
			lock.Unlock()
			return "", nil
		}
		jobs = map[string]bool{}
		for _, j := range board.Jobs {
			jobs[j.ID] = true
		}
		cache.putAshbyBoard(e.Slug, jobs)
	}
	lock.Unlock()
	if jobs[e.JobID] {
		return "open", nil
	}
	return "closed", nil
}

// checkGem probes the posting's own apply page rather than Gem's GraphQL
// board-list API: a board whose vanity slug has been retired on Gem's
// frontend (jobs.gem.com/<slug> 404s) can still resolve on the GraphQL API
// with the old postings intact (blaze-ai and arlo confirmed live 2026-07-10,
// API returns 200 with postings while every one of that board's apply pages
// 404s), so a board-membership check against the API would report those
// postings open when they are not reachable at all. The direct page is the
// only ground truth Gem actually keeps in sync.
func checkGem(ctx context.Context, fetch Fetcher, e Eligibility) (string, error) {
	pageURL := "https://jobs.gem.com/" + e.Slug + "/" + e.JobID
	res, err := fetch(ctx, pageURL)
	if err != nil || res == nil {
		return "", err
	}
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusGone {
		return "closed", nil
	}
	return "open", nil
}

// Run checks all active jobs and marks closed rows in the repository.
func Run(ctx context.Context, repo *db.Repository, cfg Config) (Result, error) {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = defaultConcurrency
	}
	if cfg.Delay == 0 {
		cfg.Delay = defaultDelay
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	fetch := cfg.Fetch
	if fetch == nil {
		fetch = defaultFetcher(nil)
	}
	cache := cfg.Cache
	if cache == nil {
		cache = NewCache()
	}
	jobs, err := repo.CloseCheckJobs()
	if err != nil {
		return Result{}, err
	}

	var result Result
	for start := 0; start < len(jobs); start += cfg.Concurrency {
		end := start + cfg.Concurrency
		if end > len(jobs) {
			end = len(jobs)
		}
		batch := jobs[start:end]
		type checked struct {
			status string
			err    error
		}
		checkedRows := make([]checked, len(batch))
		var wg sync.WaitGroup
		for i, job := range batch {
			wg.Add(1)
			go func(i int, job db.CloseCheckJob) {
				defer wg.Done()
				status, err := CheckJob(ctx, fetch, job, cache)
				checkedRows[i] = checked{status: status, err: err}
			}(i, job)
		}
		wg.Wait()

		for i, checked := range checkedRows {
			if checked.err != nil {
				// A single probe failure (timeout/DNS/transient 5xx) must not
				// abort the whole pass: jobs are checked in a fixed order, so
				// aborting means every job after the flaky one is never checked.
				// Count it and move on; the row stays in its current state.
				result.Errored++
				continue
			}
			switch checked.status {
			case "":
				result.Skipped++
			case "closed":
				// Same reasoning as the probe-failure branch above: one failed
				// write must not discard the rest of the pass.
				if err := repo.SetPipelineStage(batch[i].ID, "closed"); err != nil {
					cfg.Log.Warn("closed check: marking job closed failed", "job", batch[i].ID, "error", err)
					result.Errored++
					continue
				}
				result.Closed++
				result.Checked++
			default:
				result.Checked++
			}
		}

		if end < len(jobs) {
			select {
			case <-time.After(cfg.Delay):
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		}
	}
	return result, nil
}
