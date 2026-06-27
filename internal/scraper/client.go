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

// Health probes the worker's GET /health.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
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
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/scrape", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	middleware.InjectTraceHeader(req)

	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("worker /scrape returned %d", res.StatusCode)
	}

	var decoded ScrapeResponse
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode scrape response: %w", err)
	}
	return decoded.Jobs, nil
}
