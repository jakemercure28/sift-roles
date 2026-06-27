package dashboard

import (
	"encoding/json"

	"job-search-automation/internal/db"
)

// This file ports renderDashboard (the full HTML page) from lib/dashboard-html.js.
// Verified byte-for-byte by the page-all.html.golden.

// PageView is the data the full page needs. BodyHTML is the pre-rendered body
// (job table for list views); the page wraps it with the banners.
type PageView struct {
	Filter    string
	Sort      string
	Search    SearchOptions
	Stats     db.Stats
	Prefs     LocationPrefs
	Heartbeat *db.Heartbeat
	BodyHTML  string

	// AuthEnabled gates the hosted login flow. When false (self-host) no
	// supabase-js / auth.js is emitted and the page is byte-for-byte unchanged.
	AuthEnabled     bool
	SupabaseURL     string
	SupabaseAnonKey string
}

const pageThemePrepaint = `<script>
// Apply the saved theme before first paint so there is no flash. With no saved
// choice we leave data-theme unset so the prefers-color-scheme media query in
// the theme stylesheet drives the first render (follow system).
(function() {
  try {
    var saved = localStorage.getItem('dashboard-theme');
    if (saved === 'light' || saved === 'dark') {
      document.documentElement.setAttribute('data-theme', saved);
    }
  } catch (e) {}
})();
</script>`

const pageChartLoader = `<script>
window.loadDashboardChart = function(callback) {
  if (window.Chart) { callback(); return; }
  if (window._dashboardChartLoading) {
    window._dashboardChartCallbacks.push(callback);
    return;
  }
  window._dashboardChartLoading = true;
  window._dashboardChartCallbacks = [callback];
  var script = document.createElement('script');
  script.src = 'https://cdn.jsdelivr.net/npm/chart.js@4/dist/chart.umd.min.js';
  script.defer = true;
  script.onload = function() {
    var callbacks = window._dashboardChartCallbacks || [];
    window._dashboardChartCallbacks = [];
    callbacks.forEach(function(fn) { fn(); });
  };
  script.onerror = function() {
    window._dashboardChartLoading = false;
    window._dashboardChartCallbacks = [];
  };
  document.head.appendChild(script);
};
</script>`

const scoringBannerSVG = `<svg xmlns="http://www.w3.org/2000/svg" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/></svg>`

// defer lets the parser finish the document instead of blocking on each script;
// they live at the end of <body> and only touch the already-parsed DOM, and defer
// preserves their document order, so behavior is unchanged. auth.js (emitted just
// above, non-deferred) still runs first.
const pageScripts = `<script defer src="/public/js/nav.js?v=11"></script>
<script defer src="/public/js/ui.js?v=11"></script>
<script defer src="/public/js/pipeline.js?v=2"></script>
<script defer src="/public/js/wizard.js?v=9"></script>
<script defer src="/public/js/settings.js?v=9"></script>
<script defer src="/public/js/effects.js?v=13"></script>`

// RenderPage ports renderDashboard.
func RenderPage(v PageView) string {
	searchHidden := ""
	if v.Filter == "market-research" || v.Filter == "analytics" {
		searchHidden = " hidden"
	}
	// In hosted mode the document load is always the empty "" tenant (the bearer
	// never rides a top-level navigation), so paint a skeleton into the three
	// regions auth.js hydrates instead of flashing 0 counts / "All locations" /
	// no jobs. Self-host renders real data inline. See skeleton.go.
	titleHTML := renderDashboardTitle(v.Filter, v.Stats, v.Heartbeat)
	filtersHTML := renderFilters(v.Filter, v.Sort, v.Search, v.Prefs, metroOrdered)
	mainHTML := renderDashboardMainContent(v.BodyHTML, v.Heartbeat)
	if v.AuthEnabled {
		titleHTML = renderSkeletonTitle()
		filtersHTML = renderSkeletonFilters()
		mainHTML = renderSkeletonMain()
	}

	return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
` + BrandFaviconLink() + `
<title>` + BrandName + `</title>
<link rel="stylesheet" href="/public/dashboard.css?v=76">
<style id="theme-vars">` + RenderThemeCSS() + `</style>
` + pageThemePrepaint + `
` + pageChartLoader + `
</head>
<body>
<div class="app-shell" id="app-shell">
` + renderSidebar(v.Filter, v.Sort, v.Search, v.AuthEnabled) + `
<div class="sidebar-backdrop" onclick="toggleSidebar()" aria-hidden="true"></div>
<main class="content">
  <div class="content-header">
    <button class="content-menu-btn" type="button" onclick="toggleSidebar()" aria-label="Open menu">` + iconMenu + `</button>
    <div class="content-headline" id="dashboard-title">
      ` + titleHTML + `
    </div>
    <div class="content-tools">
      ` + filtersHTML + `
      <input class="search-box" type="text" placeholder="Search jobs, companies, roles…" value="` + escapeHTML(normalizeViewOptions(v.Search).Q) + `" oninput="applyFilters()"` + searchHidden + ` />
    </div>
  </div>
  <div id="scoring-progress-banner" class="scoring-banner" style="display:none">
    <span class="scoring-banner-icon" aria-hidden="true">` + scoringBannerSVG + `</span>
    <span id="scoring-progress-msg"></span>
  </div>
  <div id="dashboard-main">
` + mainHTML + `
  </div>
</main>
</div>
<div class="toast" id="toast"></div>
` + renderModals(v) + `
<script>window.__THEME__ = ` + ClientThemeJSON() + `;</script>` + renderAuthBlock(v) + `
` + pageScripts + `
</body>
</html>`
}

// renderAuthBlock emits the supabase-js bootstrap (config + SDK + auth.js) only in
// hosted mode. In self-host (AuthEnabled false) it returns "" so the page is
// byte-for-byte identical to before auth existed and the golden test still holds.
// auth.js loads before pageScripts so its fetch wrapper is installed before any
// data-fetching script runs.
func renderAuthBlock(v PageView) string {
	if !v.AuthEnabled {
		return ""
	}
	cfg, _ := json.Marshal(map[string]any{
		"enabled": true,
		"url":     v.SupabaseURL,
		"anonKey": v.SupabaseAnonKey,
	})
	brand, _ := json.Marshal(BrandName)
	return "\n<script>window.__SUPABASE__ = " + string(cfg) + "; window.__BRAND__ = " + string(brand) + `;</script>
<script src="https://cdn.jsdelivr.net/npm/@supabase/supabase-js@2"></script>
<script src="/public/js/auth.js?v=8"></script>`
}
