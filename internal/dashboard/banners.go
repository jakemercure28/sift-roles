package dashboard

import (
	"math"
	"strconv"
	"strings"
	"time"

	"job-search-automation/internal/db"
)

// This file renders the dashboard banners and renderDashboardMainContent.
//
// The scraper-heartbeat banner is rendered fully. The slug-health and jd-health
// banners (driven by optional dev cache files) currently render only their empty
// state; buildListView passes nil for them, so they are inert until those richer
// loaders are ported. They never appear in the common path.

// scrapeStaleHours is the SCRAPE_STALE_HOURS default (~2 missed 6h scrape cycles).
const scrapeStaleHours = 13

// timeAgo is the compact relative-time string (e.g. "5m ago", "2h ago").
func timeAgo(iso string) string {
	if iso == "" {
		return ""
	}
	then, ok := parseHeartbeatTime(iso)
	if !ok {
		return ""
	}
	diff := time.Since(then)
	if diff < time.Minute {
		return "just now"
	}
	mins := int(diff.Minutes())
	if mins < 60 {
		return strconv.Itoa(mins) + "m ago"
	}
	hours := mins / 60
	if hours < 24 {
		return strconv.Itoa(hours) + "h ago"
	}
	return strconv.Itoa(hours/24) + "d ago"
}

// hoursSince returns whole-ish hours since iso, or +Inf when unset/invalid.
func hoursSince(iso string) float64 {
	if iso == "" {
		return math.Inf(1)
	}
	then, ok := parseHeartbeatTime(iso)
	if !ok {
		return math.Inf(1)
	}
	return time.Since(then).Hours()
}

// parseHeartbeatTime parses the RFC3339 timestamps the Go scheduler writes.
func parseHeartbeatTime(iso string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, iso); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339Nano, iso); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// renderScraperHeartbeatBanner ports renderScraperHeartbeatBanner. hb nil => "".
func renderScraperHeartbeatBanner(hb *db.Heartbeat) string {
	if hb == nil {
		return ""
	}
	// A failed scrape run is operator detail (a deploy restart trips it
	// mid-cycle): it stays in the logs and the heartbeat row, never in
	// tenant-facing HTML. The banner only reports the one thing a tenant can
	// act on, listings that are actually going stale, so an errored heartbeat
	// with a recent success renders nothing.
	lastSuccess := hb.LastSuccessAt
	if lastSuccess == "" {
		return ""
	}
	staleH := hoursSince(lastSuccess)
	if staleH <= scrapeStaleHours || math.IsInf(staleH, 1) {
		return ""
	}
	msg := `<strong>No successful scrape in ` + strconv.Itoa(int(math.Floor(staleH))) + `h.</strong> Job listings may be stale.`
	lastGood := `<span style="color:var(--text-muted);margin-left:4px">Last good: ` + timeAgo(lastSuccess) + `</span>`
	// Status only, no action: scrape health is the operator's to fix, and in
	// global mode a per-tenant re-scrape can't refresh the shared board pass
	// this banner reports on anyway.
	return `<div style="background:var(--amber-glow);border:1px solid var(--amber);padding:8px 16px;margin:8px 16px;border-radius:6px;font-size:13px;color:var(--amber);display:flex;align-items:center;gap:8px">
  <span style="font-size:16px">&#9888;</span>
  <span>` + msg + ` ` + lastGood + `</span>
</div>`
}

// renderSlugHealthBanner / renderJdHealthBanner: empty state only for now (the
// rich loaders are not ported yet; buildListView passes nil).
func renderSlugHealthBanner() string { return "" }
func renderJdHealthBanner() string   { return "" }

// renderStaleResumeBanner reports how many kept pending scores were computed
// against an older resume (resume edits keep low/mid scores instead of
// re-spending the whole scoring budget; only the high band re-queues). Quiet
// informational chrome: neutral colors, no action button, n <= 0 renders "".
func renderStaleResumeBanner(n int) string {
	if n <= 0 {
		return ""
	}
	return `<div style="background:var(--bg-secondary);border:1px solid var(--border);padding:8px 16px;margin:8px 16px;border-radius:6px;font-size:13px;color:var(--text-muted);display:flex;align-items:center;gap:8px">
  <span style="font-size:16px">&#8635;</span>
  <span><strong>` + strconv.Itoa(n) + ` scores reflect an older resume.</strong> New listings and strong matches are re-scored against your latest resume automatically.</span>
</div>`
}

// renderDashboardMainContent ports renderDashboardMainContent: the banners
// then the body. With all banners empty this is "\n\n" + bodyHtml.
func renderDashboardMainContent(bodyHTML string, hb *db.Heartbeat, staleScores int) string {
	var b strings.Builder
	b.WriteString(renderScraperHeartbeatBanner(hb))
	b.WriteString(renderStaleResumeBanner(staleScores))
	b.WriteString(renderJdHealthBanner())
	b.WriteString("\n")
	b.WriteString(renderSlugHealthBanner())
	b.WriteString("\n")
	b.WriteString(bodyHTML)
	return b.String()
}
