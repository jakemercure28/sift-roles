package dashboard

import (
	"io"
	"strings"
	"testing"

	"job-search-automation/internal/db"
)

func TestDashboardPageNative(t *testing.T) {
	ts, _, conn := fragmentServer(t)
	seedFullJob(t, conn, db.ListedJob{ID: "gh-1", Title: "Platform Engineer", Company: "Acme", Status: "pending", Score: ptr(9), PostedAt: "2026-06-01", Platform: "greenhouse"})

	resp := get(t, ts.URL+"/", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	for _, want := range []string{"<!DOCTYPE html>", "<title>" + BrandName + "</title>", `id="theme-vars"`, `data-id="gh-1"`, `id="app-sidebar"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("page missing %q", want)
		}
	}
}

func TestDashboardPageReportViewsNative(t *testing.T) {
	ts, _, _ := fragmentServer(t)

	resp := get(t, ts.URL+"/?filter=market-research", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `class="market-report"`) {
		t.Fatalf("market-research page not native: %q", string(body)[:min(80, len(body))])
	}

	// analytics renders natively (full page, not the proxy marker).
	resp = get(t, ts.URL+"/?filter=analytics", nil)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `class="analytics-report"`) {
		t.Fatalf("analytics page not native: %q", string(body)[:min(80, len(body))])
	}
}

func TestDashboardPageLocationWritesNative(t *testing.T) {
	ts, _, _ := fragmentServer(t)

	resp := get(t, ts.URL+"/?setMetro=seattle&setUnlisted=0&setRemote=1", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "<!DOCTYPE html>") {
		t.Fatalf("location write did not render native page: %q", body)
	}
}
