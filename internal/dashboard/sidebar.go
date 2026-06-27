package dashboard

import "strings"

// This file ports the sidebar from lib/dashboard-html.js (NAV_ICONS, navItem,
// renderSidebar). Verified in context by the full-page golden.

const navSW = `fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"`

var navIcons = map[string]string{
	"jobs":         `<svg class="nav-icon" width="18" height="18" viewBox="0 0 24 24" ` + navSW + `><path d="M3 10.5 12 3l9 7.5"/><path d="M5 9.5V21h14V9.5"/></svg>`,
	"applications": `<svg class="nav-icon" width="18" height="18" viewBox="0 0 24 24" ` + navSW + `><path d="M22 2 11 13"/><path d="M22 2 15 22 11 13 2 9z"/></svg>`,
	"interviews":   `<svg class="nav-icon" width="18" height="18" viewBox="0 0 24 24" ` + navSW + `><rect x="3" y="4" width="18" height="18" rx="2"/><path d="M16 2v4M8 2v4M3 10h18"/></svg>`,
	"offers":       `<svg class="nav-icon" width="18" height="18" viewBox="0 0 24 24" ` + navSW + `><path d="M12 2.5 15 9l7 .6-5.3 4.6L18.4 21 12 17.3 5.6 21 7.3 14.2 2 9.6 9 9z"/></svg>`,
	"archive":      `<svg class="nav-icon" width="18" height="18" viewBox="0 0 24 24" ` + navSW + `><rect x="3" y="4" width="18" height="4" rx="1"/><path d="M5 8v12h14V8"/><path d="M10 12h4"/></svg>`,
	"analytics":    `<svg class="nav-icon" width="18" height="18" viewBox="0 0 24 24" ` + navSW + `><path d="M3 3v18h18"/><path d="M7 16v-4M12 16V8M17 16v-7"/></svg>`,
	"reports":      `<svg class="nav-icon" width="18" height="18" viewBox="0 0 24 24" ` + navSW + `><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/><path d="M8 13h8M8 17h8M8 9h2"/></svg>`,
	"log":          `<svg class="nav-icon" width="18" height="18" viewBox="0 0 24 24" ` + navSW + `><path d="M8 6h13M8 12h13M8 18h13"/><path d="M3 6h.01M3 12h.01M3 18h.01"/></svg>`,
	"settings":     `<svg class="nav-icon" width="18" height="18" viewBox="0 0 24 24" ` + navSW + `><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/></svg>`,
	"themeMoon":    `<svg id="theme-icon-moon" class="nav-icon" width="18" height="18" viewBox="0 0 24 24" ` + navSW + `><path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z"/></svg>`,
	"themeSun":     `<svg id="theme-icon-sun" class="nav-icon" width="18" height="18" viewBox="0 0 24 24" ` + navSW + ` style="display:none"><circle cx="12" cy="12" r="5"/><line x1="12" y1="1" x2="12" y2="3"/><line x1="12" y1="21" x2="12" y2="23"/><line x1="4.22" y1="4.22" x2="5.64" y2="5.64"/><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"/><line x1="1" y1="12" x2="3" y2="12"/><line x1="21" y1="12" x2="23" y2="12"/><line x1="4.22" y1="19.78" x2="5.64" y2="18.36"/><line x1="18.36" y1="5.64" x2="19.78" y2="4.22"/></svg>`,
	"signout":      `<svg class="nav-icon" width="18" height="18" viewBox="0 0 24 24" ` + navSW + `><path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><path d="M16 17l5-5-5-5"/><path d="M21 12H9"/></svg>`,
}

const iconMenu = `<svg width="18" height="18" viewBox="0 0 24 24" ` + navSW + `><line x1="3" y1="6" x2="21" y2="6"/><line x1="3" y1="12" x2="21" y2="12"/><line x1="3" y1="18" x2="21" y2="18"/></svg>`
const iconClose = `<svg width="16" height="16" viewBox="0 0 24 24" ` + navSW + `><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg>`

// signOutNavItem emits the sidebar sign-out action, only in hosted auth mode.
// It calls window.dashboardSignOut (public/js/auth.js), which is defined only
// when auth.js loads (hosted). In self-host it returns "" so the sidebar is
// byte-for-byte identical to before.
func signOutNavItem(authEnabled bool) string {
	if !authEnabled {
		return ""
	}
	return "\n      " + `<button class="nav-item nav-item--signout" type="button" onclick="dashboardSignOut()">` + navIcons["signout"] + `<span class="nav-label">Sign out</span></button>`
}

func navItem(id, label, iconKey, filter, sortKey string, opts SearchOptions) string {
	return `<a class="nav-item` + activeIf(filter == id) + `" href="` + buildDashboardHref(id, sortKey, opts) + `" data-filter="` + id + `">` + navIcons[iconKey] + `<span class="nav-label">` + label + `</span></a>`
}

func renderSidebar(filter, sortKey string, opts SearchOptions, authEnabled bool) string {
	moreOpen := ""
	if filter == "all" || filter == "rejected" || filter == "closed" || filter == "ghosted" {
		moreOpen = " open"
	}
	var b strings.Builder
	b.WriteString(`<aside class="sidebar" id="app-sidebar">
  <div class="sidebar-head">
    <span class="sidebar-brand" aria-label="` + BrandName + `">` + BrandWordmarkSVG() + `<span class="sidebar-brand-compact">` + BrandMarkSVG() + `</span></span>
    <button class="sidebar-close" type="button" onclick="toggleSidebar()" aria-label="Close menu">` + iconClose + `</button>
  </div>
  <nav class="sidebar-nav">
    <div class="nav-group">
      ` + navItem("not-applied", "Jobs", "jobs", filter, sortKey, opts) + `
      ` + navItem("applied", "Applications", "applications", filter, sortKey, opts) + `
      ` + navItem("interviewing", "Interviews", "interviews", filter, sortKey, opts) + `
      ` + navItem("offers", "Offers", "offers", filter, sortKey, opts) + `
    </div>
    <div class="nav-group">
      ` + navItem("analytics", "Analytics", "analytics", filter, sortKey, opts) + `
      ` + navItem("market-research", "Market Research", "reports", filter, sortKey, opts) + `
      ` + navItem("activity-log", "Event Log", "log", filter, sortKey, opts) + `
    </div>
    <details class="nav-more"` + moreOpen + `>
      <summary class="nav-more-summary">More<svg class="nav-more-caret" width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polyline points="6 9 12 15 18 9"/></svg></summary>
      <div class="nav-more-items">
        <a class="nav-item nav-subitem` + activeIf(filter == "all") + `" href="` + buildDashboardHref("all", sortKey, opts) + `" data-filter="all"><span class="nav-label">All Jobs</span></a>
        <a class="nav-item nav-subitem` + activeIf(filter == "archived") + `" href="` + buildDashboardHref("archived", sortKey, opts) + `" data-filter="archived"><span class="nav-label">Archive</span></a>
        <a class="nav-item nav-subitem` + activeIf(filter == "rejected") + `" href="` + buildDashboardHref("rejected", "date", opts) + `" data-filter="rejected"><span class="nav-label">Rejected</span></a>
        <a class="nav-item nav-subitem` + activeIf(filter == "closed") + `" href="` + buildDashboardHref("closed", sortKey, opts) + `" data-filter="closed"><span class="nav-label">Closed</span></a>
        <a class="nav-item nav-subitem` + activeIf(filter == "ghosted") + `" href="` + buildDashboardHref("ghosted", sortKey, opts) + `" data-filter="ghosted"><span class="nav-label">Ghosted</span></a>
        <a class="nav-item nav-subitem" href="/report-problem"><span class="nav-label">Report Problem</span></a>
      </div>
    </details>
    <div class="nav-spacer"></div>
    <div class="nav-group">
      <button class="nav-item" type="button" onclick="openSettings()">` + navIcons["settings"] + `<span class="nav-label">Settings</span></button>
      <button class="nav-item" type="button" onclick="toggleTheme()" aria-label="Toggle theme">` + navIcons["themeMoon"] + navIcons["themeSun"] + `<span class="nav-label">Theme</span></button>` + signOutNavItem(authEnabled) + `
    </div>
  </nav>
</aside>`)
	return b.String()
}
