package dashboard

import (
	"strings"
	"testing"
	"time"

	"job-search-automation/internal/db"
)

// An errored heartbeat with a recent success is operator noise (a deploy
// restart trips it mid-cycle): it must render nothing tenant-facing.
func TestScraperHeartbeatBannerHidesTransientError(t *testing.T) {
	hb := &db.Heartbeat{
		Status:        "error",
		Error:         `Post "http://scraper-service:4040/scrape": context deadline exceeded`,
		LastSuccessAt: time.Now().Add(-4 * time.Hour).UTC().Format(time.RFC3339),
	}
	if html := renderScraperHeartbeatBanner(hb); html != "" {
		t.Fatalf("transient scrape error must not render a banner: %s", html)
	}
}

// Genuine staleness (no successful scrape past the threshold) still warns,
// without leaking raw error detail (transport errors, internal hostnames).
func TestScraperHeartbeatBannerWarnsWhenStale(t *testing.T) {
	hb := &db.Heartbeat{
		Status:        "error",
		Error:         `Post "http://scraper-service:4040/scrape": context deadline exceeded`,
		LastSuccessAt: time.Now().Add(-20 * time.Hour).UTC().Format(time.RFC3339),
	}
	html := renderScraperHeartbeatBanner(hb)
	if !strings.Contains(html, "Job listings may be stale.") {
		t.Fatalf("banner missing staleness message: %s", html)
	}
	if strings.Contains(html, "scraper-service") || strings.Contains(html, "context deadline") {
		t.Fatalf("banner leaks raw error detail: %s", html)
	}
	if !strings.Contains(html, "Last good:") {
		t.Fatalf("banner missing last-good hint: %s", html)
	}
}

// A heartbeat that never recorded a success has no staleness reference point;
// that state is the operator's to notice, not the tenant's.
func TestScraperHeartbeatBannerSilentWithoutSuccess(t *testing.T) {
	hb := &db.Heartbeat{Status: "error", Error: "boom"}
	if html := renderScraperHeartbeatBanner(hb); html != "" {
		t.Fatalf("heartbeat without a success must not render a banner: %s", html)
	}
}
