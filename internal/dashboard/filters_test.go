package dashboard

import (
	"strings"
	"testing"
	"time"

	"job-search-automation/internal/db"
)

func defaultLocPrefs() LocationPrefs {
	return LocationPrefs{Metros: []string{}, IncludeUnknown: true, RemoteOnly: false}
}

func TestRenderFiltersParity(t *testing.T) {
	assertGolden(t, "filters-all.html.golden",
		renderFilters("all", "score", SearchOptions{}, defaultLocPrefs(), metroOrdered))
	assertGolden(t, "filters-market.html.golden",
		renderFilters("market-research", "score", SearchOptions{}, defaultLocPrefs(), metroOrdered))
	assertGolden(t, "filters-analytics.html.golden",
		renderFilters("analytics", "score", SearchOptions{}, defaultLocPrefs(), metroOrdered))
}

func TestMetroOrderingMatchesJSON(t *testing.T) {
	if len(metroOrdered) != 12 {
		t.Fatalf("metroOrdered has %d entries, want 12", len(metroOrdered))
	}
	if metroOrdered[0].Key != "seattle" || metroOrdered[0].Label != "Seattle" {
		t.Fatalf("first metro = %+v, want seattle/Seattle", metroOrdered[0])
	}
}

func TestRenderDashboardTitle(t *testing.T) {
	stats := db.Stats{Total: 12, NotApplied: 5, Applied: 4, Interviewing: 2, Offers: 1, Rejected: 3, Closed: 1}

	cases := map[string]string{
		"all":             `<h1 class="content-title">All Jobs</h1><div class="content-sub page-status">12 total · 5 pending · 4 applied · 2 interviewing</div>`,
		"not-applied":     `<h1 class="content-title">Jobs</h1><div class="content-sub page-status">5 pending</div>`,
		"applied":         `<h1 class="content-title">Applications</h1><div class="content-sub page-status">4 applied</div>`,
		"offers":          `<h1 class="content-title">Offers</h1><div class="content-sub page-status">1 offers</div>`,
		"analytics":       `<h1 class="content-title">Analytics</h1>`,
		"market-research": `<h1 class="content-title">Market Research</h1>`,
	}
	for filter, want := range cases {
		if got := renderDashboardTitle(filter, stats, nil); got != want {
			t.Errorf("title(%q) =\n  %q\nwant\n  %q", filter, got, want)
		}
	}

	// "all" with all-zero stats drops the sub line entirely.
	if got := renderDashboardTitle("all", db.Stats{}, nil); got != `<h1 class="content-title">All Jobs</h1>` {
		t.Fatalf("zero-stats all title = %q", got)
	}
	// A count filter with a zero count still shows "0 noun".
	if got := renderDashboardTitle("rejected", db.Stats{}, nil); got != `<h1 class="content-title">Rejected</h1><div class="content-sub page-status">0 rejected</div>` {
		t.Fatalf("zero rejected title = %q", got)
	}

	// Scrape freshness is never folded into the title subtitle; it lives in the
	// dedicated scrape banner, so repeating it here was redundant.
	hb := &db.Heartbeat{Status: "ok", LastSuccessAt: time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339), Inserted: 2}
	for _, filter := range []string{"all", "not-applied", "analytics", "applied"} {
		if got := renderDashboardTitle(filter, stats, hb); strings.Contains(got, "last scrape") {
			t.Fatalf("title(%q) should not show scrape freshness: %q", filter, got)
		}
	}
}
