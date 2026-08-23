// Package scraper is the HTTP client to the TypeScript scraper worker.
package scraper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"job-search-automation/internal/middleware"
	"job-search-automation/internal/model"
)

// ScrapeResponse mirrors the worker's POST /scrape response body.
type ScrapeResponse struct {
	Count     int          `json:"count"`
	Platforms []string     `json:"platforms"`
	Jobs      []model.Lead `json:"jobs"`
	// Boards is board mode's per-board fetch outcomes (single-request
	// platforms only); absent in profileDir mode.
	Boards []BoardFetchResult `json:"boards,omitempty"`
}

// Client talks to the scraper worker.
type Client struct {
	baseURL string
	http    *http.Client
}

// New returns a Client for the worker at baseURL with the given per-request timeout.
func New(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: timeout},
	}
}

// Health probes the worker's GET /livez, the shallow "is the worker reachable"
// endpoint. Deliberately not /health: that one reports work health and answers
// 503 while a board pass overruns its stuck threshold, so probing it made a
// merely slow pass fail the caller's healthcheck (the `healthcheck` subcommand
// backs the compose container probe) and restart a perfectly healthy service.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/livez", nil)
	if err != nil {
		return err
	}
	middleware.InjectTraceHeader(req)
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("worker health returned %d", res.StatusCode)
	}
	return nil
}

// BoardRef names one explicit ATS board for board-mode scraping, mirroring the
// worker's BoardRef. Platform uses the worker's lowercase platform keys
// (greenhouse|lever|ashby|workable|workday); Workday boards additionally need
// the cluster number (WD) and board path (Board), and Label is the display
// company name.
type BoardRef struct {
	Platform string `json:"platform"`
	Slug     string `json:"slug"`
	Label    string `json:"label,omitempty"`
	WD       int    `json:"wd,omitempty"`
	Board    string `json:"board,omitempty"`
	// Etag/ContentHash carry the previous cycle's fetch state so the worker
	// can skip a board whose content did not change (If-None-Match / body
	// hash). Empty means always fetch.
	Etag        string `json:"etag,omitempty"`
	ContentHash string `json:"contentHash,omitempty"`
}

// BoardFetchResult is one board's fetch outcome in board mode, mirroring the
// worker's BoardFetchResult. Status is ok|unchanged|failed; Etag/ContentHash
// are the state to feed back on the next cycle. Company is set on Workday
// results only: the company name the board's leads are stored under, so a
// failed (incomplete) board can be excluded from the closed-by-diff pass.
type BoardFetchResult struct {
	Platform    string `json:"platform"`
	Slug        string `json:"slug"`
	Status      string `json:"status"`
	Etag        string `json:"etag,omitempty"`
	ContentHash string `json:"contentHash,omitempty"`
	Company     string `json:"company,omitempty"`
}

// ScrapeBoards calls POST /scrape in board mode: the worker fetches exactly
// these boards in full and returns ALL live postings with no tenant
// search-term filtering (term matching happens Go-side when the harvest is
// distributed to tenants). No profile files are read by the worker. The
// second return value reports per-board fetch outcomes (unchanged boards
// return no jobs and should keep their previous harvest state).
func (c *Client) ScrapeBoards(ctx context.Context, boards []BoardRef) ([]model.Lead, []BoardFetchResult, error) {
	res, err := c.postScrape(ctx, map[string]any{"boards": boards})
	if err != nil {
		return nil, nil, err
	}
	return res.Jobs, res.Boards, nil
}

// Scrape calls POST /scrape. If platforms is empty the worker runs all of them.
// profileDir is the active tenant's profile dir (companies.json + search terms);
// the worker reads its company config from there for this request. When empty
// (e.g. the worker-only scrape-test probe), the worker falls back to its own
// DATA_DIR. The dir is shared via the bind-mount, so a container-absolute path
// from the Go side resolves on the worker side.
func (c *Client) Scrape(ctx context.Context, platforms []string, profileDir string) ([]model.Lead, error) {
	body := map[string]any{"platforms": platforms}
	if profileDir != "" {
		body["profileDir"] = profileDir
	}
	res, err := c.postScrape(ctx, body)
	if err != nil {
		return nil, err
	}
	return res.Jobs, nil
}

// postScrape sends one POST /scrape request body and decodes the response.
func (c *Client) postScrape(ctx context.Context, body map[string]any) (ScrapeResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return ScrapeResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/scrape", bytes.NewReader(payload))
	if err != nil {
		return ScrapeResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	middleware.InjectTraceHeader(req)

	res, err := c.http.Do(req)
	if err != nil {
		return ScrapeResponse{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return ScrapeResponse{}, fmt.Errorf("worker /scrape returned %d", res.StatusCode)
	}

	var decoded ScrapeResponse
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return ScrapeResponse{}, fmt.Errorf("decode scrape response: %w", err)
	}
	return decoded, nil
}
