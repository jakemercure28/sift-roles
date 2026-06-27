// Package closedcheck probes canonical ATS postings and closes rows that have
// disappeared from their source board.
package closedcheck

import (
	"context"
	"encoding/json"
	"io"
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

// Config controls one check pass.
type Config struct {
	Concurrency int
	Delay       time.Duration
	Fetch       Fetcher
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
func CheckJob(ctx context.Context, fetch Fetcher, job db.CloseCheckJob, ashbyCache map[string]map[string]bool) (string, error) {
	eligibility := AutoCloseEligibility(job)
	if eligibility == nil {
		return "", nil
	}
	switch eligibility.Platform {
	case "Greenhouse":
		return checkGreenhouse(ctx, fetch, *eligibility)
	case "Ashby":
		return checkAshby(ctx, fetch, *eligibility, ashbyCache)
	}
	return "", nil
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

func checkAshby(ctx context.Context, fetch Fetcher, e Eligibility, cache map[string]map[string]bool) (string, error) {
	jobs, ok := cache[e.Slug]
	if !ok {
		res, err := fetch(ctx, "https://api.ashbyhq.com/posting-api/job-board/"+e.Slug)
		if err != nil || res == nil || res.StatusCode < 200 || res.StatusCode >= 300 {
			return "", err
		}
		var board struct {
			Jobs []struct {
				ID string `json:"id"`
			} `json:"jobs"`
		}
		if err := json.Unmarshal(res.Body, &board); err != nil {
			return "", nil
		}
		jobs = map[string]bool{}
		for _, j := range board.Jobs {
			jobs[j.ID] = true
		}
		cache[e.Slug] = jobs
	}
	if jobs[e.JobID] {
		return "open", nil
	}
	return "closed", nil
}

// Run checks all active jobs and marks closed rows in the repository.
func Run(ctx context.Context, repo *db.Repository, cfg Config) (Result, error) {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = defaultConcurrency
	}
	if cfg.Delay == 0 {
		cfg.Delay = defaultDelay
	}
	fetch := cfg.Fetch
	if fetch == nil {
		fetch = defaultFetcher(nil)
	}
	jobs, err := repo.CloseCheckJobs()
	if err != nil {
		return Result{}, err
	}

	var result Result
	ashbyCache := map[string]map[string]bool{}
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
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i, job := range batch {
			wg.Add(1)
			go func(i int, job db.CloseCheckJob) {
				defer wg.Done()
				mu.Lock()
				status, err := CheckJob(ctx, fetch, job, ashbyCache)
				mu.Unlock()
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
				if err := repo.SetPipelineStage(batch[i].ID, "closed"); err != nil {
					return Result{}, err
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
