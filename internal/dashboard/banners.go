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
	lastSuccess := hb.LastSuccessAt
	errored := hb.Status == "error"
	staleH := hoursSince(lastSuccess)
	stale := staleH > scrapeStaleHours

	// Healthy heartbeat now renders inside the page status line under the title
	// (renderDashboardTitle); only the error/stale warning stays a banner.
	if !errored && !stale {
		return ""
	}

	bg, border, color := "var(--amber-glow)", "var(--amber)", "var(--amber)"
	if errored {
		bg, border, color = "var(--red-glow)", "var(--red)", "var(--red)"
	}
	var msg string
	if errored {
		msg = `Last scrape <strong>failed</strong>`
		if hb.Error != "" {
			msg += " — " + escapeHTML(hb.Error)
		}
		msg += "."
	} else {
		msg = `<strong>No successful scrape in ` + strconv.Itoa(int(math.Floor(staleH))) + `h.</strong> Job listings may be stale.`
	}
	lastGood := ""
	if lastSuccess != "" {
		lastGood = `<span style="color:var(--text-muted);margin-left:4px">Last good: ` + timeAgo(lastSuccess) + `</span>`
	}
	return `<div style="background:` + bg + `;border:1px solid ` + border + `;padding:8px 16px;margin:8px 16px;border-radius:6px;font-size:13px;color:` + color + `;display:flex;align-items:center;gap:8px">
  <span style="font-size:16px">&#9888;</span>
  <span>` + msg + ` ` + lastGood + `</span>
  <span id="scrape-now-status" style="margin-left:auto;font-size:12px;color:var(--text-muted)"></span>
  <button type="button" id="scrape-now-btn" onclick="scrapeNow(this)" style="flex-shrink:0;background:transparent;border:1px solid currentColor;color:inherit;border-radius:5px;padding:4px 10px;font-size:12px;font-weight:600;cursor:pointer">Scrape now</button>
</div>`
}

// renderSlugHealthBanner / renderJdHealthBanner: empty state only for now (the
// rich loaders are not ported yet; buildListView passes nil).
func renderSlugHealthBanner() string { return "" }
func renderJdHealthBanner() string   { return "" }

// renderDashboardMainContent ports renderDashboardMainContent: the three banners
// then the body. With all banners empty this is "\n\n" + bodyHtml.
func renderDashboardMainContent(bodyHTML string, hb *db.Heartbeat) string {
	var b strings.Builder
	b.WriteString(renderScraperHeartbeatBanner(hb))
	b.WriteString(renderJdHealthBanner())
	b.WriteString("\n")
	b.WriteString(renderSlugHealthBanner())
	b.WriteString("\n")
	b.WriteString(bodyHTML)
	return b.String()
}
