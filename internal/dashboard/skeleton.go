package dashboard

import "strings"

// Hosted-mode hydration skeleton.
//
// In hosted (Supabase) mode the top-level GET / document never carries the
// bearer token — browsers don't attach it to a navigation — so the server
// always renders "/" as the empty "" tenant: 0 counts, "All locations", no
// jobs. auth.js then re-fetches the real tenant's body once the session
// resolves (see hydrateDashboard + navigateDashboardUrl, which swap the
// #dashboard-title / #dashboard-filters / #dashboard-main regions). Rendering
// the empty-tenant data on first paint is exactly the flicker users saw on
// refresh, so in hosted mode we paint a neutral skeleton into those three
// regions instead and let hydration replace it. Self-host (AuthEnabled false)
// is unaffected and keeps rendering real data inline.

// renderSkeletonTitle fills #dashboard-title. It mirrors renderDashboardTitle's
// shape (an <h1> plus the one-line status under it) so the swap to real content
// doesn't shift layout.
func renderSkeletonTitle() string {
	return `<h1 class="content-title"><span class="skeleton skel-title"></span></h1>` +
		`<div class="content-sub page-status"><span class="skeleton skel-sub"></span></div>`
}

// renderSkeletonFilters fills the #dashboard-filters slot. The id must match so
// nav.js's outerHTML swap finds and replaces it during hydration.
func renderSkeletonFilters() string {
	return "\n<div class=\"content-filters\" id=\"dashboard-filters\">\n  " +
		`<span class="skeleton skel-pill"></span>` + "\n  " +
		`<span class="skeleton skel-pill"></span>` + "\n</div>"
}

// renderSkeletonMain fills #dashboard-main with placeholder job rows inside the
// same .job-list-panel wrapper the real list uses.
func renderSkeletonMain() string {
	var b strings.Builder
	b.WriteString(`<div class="job-list-panel" aria-busy="true">`)
	for i := 0; i < 8; i++ {
		b.WriteString(`<div class="skel-row"><span class="skeleton skel-row-title"></span><span class="skeleton skel-row-meta"></span></div>`)
	}
	b.WriteString(`</div>`)
	return b.String()
}
