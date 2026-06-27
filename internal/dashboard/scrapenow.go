package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// scrapeNowResp is the success shape returned by handleScrapeNow. Failures
// (unset trigger, unreachable, busy) go through the shared error helpers.
type scrapeNowResp struct {
	OK bool `json:"ok"`
}

// Sentinel errors so callers can distinguish a missing trigger (deployment not
// wired up) from a reachable-but-failing one.
var (
	errScrapeTriggerUnset = errors.New("Scraper trigger not configured")
	errScrapeUnreachable  = errors.New("Scraper unreachable")
)

// triggerScrape POSTs to the Go scrape trigger (SCRAPE_TRIGGER_URL/scrape) and
// normalizes the outcome. busy is true when a scrape is already running; a nil
// error with busy=false means a fresh scrape was accepted. Both the "Scrape
// now" button and the onboarding wizard's first run go through here.
func (s *Server) triggerScrape(ctx context.Context) (busy bool, err error) {
	if s.scrapeTriggerURL == "" {
		return false, errScrapeTriggerUnset
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url := strings.TrimRight(s.scrapeTriggerURL, "/") + "/scrape"
	payload, err := json.Marshal(map[string]string{"userId": s.repo.UserID()})
	if err != nil {
		return false, errScrapeUnreachable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return false, errScrapeUnreachable
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.log.Warn("scrape trigger unreachable", "error", err)
		return false, errScrapeUnreachable
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusConflict:
		return true, nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		s.log.Warn("scrape trigger returned non-ok", "status", resp.StatusCode)
		return false, fmt.Errorf("Scraper returned %d", resp.StatusCode)
	default:
		return false, nil
	}
}

// handleScrapeNow fires a scrape on demand for the dashboard's "Scrape now"
// button, mapping the trigger outcome to the legacy JSON + status codes.
func (s *Server) handleScrapeNow(w http.ResponseWriter, r *http.Request) {
	busy, err := s.triggerScrape(r.Context())
	switch {
	case errors.Is(err, errScrapeTriggerUnset):
		jsonError(w, http.StatusServiceUnavailable, err.Error())
	case busy:
		jsonBusy(w)
	case err != nil:
		jsonError(w, http.StatusBadGateway, err.Error())
	default:
		writeJSON(w, http.StatusOK, scrapeNowResp{OK: true})
	}
}
