package dashboard

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"job-search-automation/internal/db"
)

// fragmentServer starts the dashboard over a seeded repo.
func fragmentServer(t *testing.T) (*httptest.Server, *db.Repository, *sql.DB) {
	t.Helper()
	repo, conn := dataLayerRepo(t)
	srv, err := New(t.TempDir(), repo, nil, time.Second, 500, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, repo, conn
}

func getFragment(t *testing.T, ts *httptest.Server, query string) fragmentResponse {
	t.Helper()
	resp := get(t, ts.URL+"/api/dashboard-list?"+query, nil)
	defer resp.Body.Close()
	var f fragmentResponse
	if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
		t.Fatalf("decode fragment: %v", err)
	}
	return f
}

func TestDashboardListFragmentNative(t *testing.T) {
	ts, repo, conn := fragmentServer(t)
	seedFullJob(t, conn, db.ListedJob{ID: "gh-1", Title: "Platform Engineer", Company: "Acme", Status: "pending", Score: ptr(9), PostedAt: "2026-06-01", Platform: "greenhouse"})
	seedFullJob(t, conn, db.ListedJob{ID: "gh-2", Title: "SRE", Company: "Globex", Status: "pending", Score: ptr(7), PostedAt: "2026-05-20", Platform: "greenhouse"})

	f := getFragment(t, ts, "filter=all")
	if !f.OK {
		t.Fatal("ok=false")
	}
	if f.URL != "/?filter=all&sort=score" {
		t.Fatalf("url = %q", f.URL)
	}

	// Title + filters must equal the (separately parity-verified) renderers.
	stats, _ := repo.GlobalStats()
	if want := renderDashboardTitle("all", stats, nil); f.TitleHTML != want {
		t.Fatalf("titleHtml mismatch:\n got %q\nwant %q", f.TitleHTML, want)
	}
	wantFilters := renderFilters("all", "score", SearchOptions{MinScore: 1, Page: 1}, defaultLocPrefs(), metroOrdered)
	if f.FiltersHTML != wantFilters {
		t.Fatalf("filtersHtml mismatch")
	}

	// With no heartbeat or health banners, mainHtml is "\n\n" + the job table.
	if !strings.HasPrefix(f.MainHTML, "\n\n<div class=\"job-list-panel\">") {
		t.Fatalf("mainHtml prefix = %q", f.MainHTML[:40])
	}
	if !strings.Contains(f.MainHTML, `data-id="gh-1"`) || !strings.Contains(f.MainHTML, `data-id="gh-2"`) {
		t.Fatal("mainHtml missing seeded jobs")
	}
}

func TestDashboardListReportViewsNative(t *testing.T) {
	ts, _, _ := fragmentServer(t)
	for _, c := range []struct{ filter, title string }{
		{"analytics", "Analytics"},
		{"activity-log", "Event Log"},
		{"market-research", "Market Research"},
	} {
		f := getFragment(t, ts, "filter="+c.filter)
		if !strings.Contains(f.TitleHTML, c.title) {
			t.Fatalf("%s title = %q, want native %q", c.filter, f.TitleHTML, c.title)
		}
		if c.filter == "market-research" && !strings.Contains(f.MainHTML, `class="market-report"`) {
			t.Fatalf("market-research mainHtml not native: %q", f.MainHTML[:min(80, len(f.MainHTML))])
		}
	}
}

func TestDashboardListRejectedDefaultsToDateSort(t *testing.T) {
	ts, _, conn := fragmentServer(t)
	seedFullJob(t, conn, db.ListedJob{ID: "r1", Title: "X", Company: "Y", Status: "rejected", Stage: "rejected", Score: ptr(5)})

	f := getFragment(t, ts, "filter=rejected")
	if f.URL != "/?filter=rejected&sort=date" {
		t.Fatalf("rejected url = %q, want sort=date default", f.URL)
	}
	// An explicit sort overrides the default.
	f2 := getFragment(t, ts, "filter=rejected&sort=score")
	if f2.URL != "/?filter=rejected&sort=score" {
		t.Fatalf("explicit sort url = %q", f2.URL)
	}
}

func TestDashboardListSearchAndPaging(t *testing.T) {
	ts, _, conn := fragmentServer(t)
	seedFullJob(t, conn, db.ListedJob{ID: "a", Title: "Platform Engineer", Company: "Acme", Status: "pending", Score: ptr(9)})
	seedFullJob(t, conn, db.ListedJob{ID: "b", Title: "Frontend Dev", Company: "Globex", Status: "pending", Score: ptr(9)})

	f := getFragment(t, ts, "filter=all&q=acme")
	if !strings.Contains(f.MainHTML, `data-id="a"`) || strings.Contains(f.MainHTML, `data-id="b"`) {
		t.Fatal("search q=acme should keep a, drop b")
	}
	if f.URL != "/?filter=all&sort=score&q=acme" {
		t.Fatalf("search url = %q", f.URL)
	}
}

func TestDashboardListHeartbeatBanner(t *testing.T) {
	ts, repo, conn := fragmentServer(t)
	seedFullJob(t, conn, db.ListedJob{ID: "a", Title: "X", Company: "Y", Status: "pending", Score: ptr(8)})
	// A recent successful heartbeat => folded into the title status line, with
	// no banner in the main body (only error/stale states render a banner).
	if err := repo.WriteHeartbeat("ok", 10, 3, ""); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	f := getFragment(t, ts, "filter=all")
	if !strings.Contains(f.TitleHTML, "last scrape") {
		t.Fatalf("titleHtml missing heartbeat status:\n%s", f.TitleHTML)
	}
	if !strings.Contains(f.TitleHTML, "3 new") {
		t.Fatal("title status line should show inserted count")
	}
	if strings.Contains(f.MainHTML, "Last scrape") {
		t.Fatal("healthy heartbeat should no longer render a main-body banner")
	}
}
