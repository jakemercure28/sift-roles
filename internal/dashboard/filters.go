package dashboard

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"job-search-automation/internal/db"
)

func itoa(n int) string { return strconv.Itoa(n) }

// This file ports lib/html/filters.js (the header Location + Sort controls) and
// renderDashboardTitle from lib/dashboard-html.js. renderFilters output is
// verified byte-for-byte against Node (filters_test.go golden).

// metroOption is a (key, label) pair preserving metros.json insertion order, so
// the location dropdown lists metros in the same order Node's Object.entries does.
type metroOption struct{ Key, Label string }

// metroOrdered parses the embedded metros.json preserving key order.
var metroOrdered = func() []metroOption {
	dec := json.NewDecoder(bytes.NewReader(metrosJSON))
	// consume opening '{'
	if _, err := dec.Token(); err != nil {
		return nil
	}
	var out []metroOption
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return out
		}
		key, _ := keyTok.(string)
		var m struct {
			Label string `json:"label"`
		}
		if err := dec.Decode(&m); err != nil {
			return out
		}
		label := m.Label
		if label == "" {
			label = key
		}
		out = append(out, metroOption{Key: key, Label: label})
	}
	return out
}()

func renderLocationDropdown(prefs LocationPrefs, metros []metroOption) string {
	if len(metros) == 0 {
		return ""
	}
	selectedMetros := map[string]struct{}{}
	for _, k := range prefs.Metros {
		selectedMetros[k] = struct{}{}
	}
	includeUnknown := prefs.IncludeUnknown
	remoteOnly := prefs.RemoteOnly
	selectedKey := ""
	if len(prefs.Metros) > 0 {
		selectedKey = prefs.Metros[0]
	}

	triggerLabel := "All locations"
	if selectedKey != "" {
		// label of the selected metro, falling back to the key.
		triggerLabel = escapeHTML(metroLabel(metros, selectedKey))
	}

	var rows strings.Builder
	rows.WriteString(`<button class="menu-item` + activeIf(selectedKey == "") + `" onclick="setLocationPref('')">All locations</button>`)
	for _, m := range metros {
		rows.WriteString(`<button class="menu-item` + activeIf(m.Key == selectedKey) + `" onclick="setLocationPref('` + escapeHTML(m.Key) + `')">` + escapeHTML(m.Label) + `</button>`)
	}

	remoteToggle := `<div class="menu-divider"></div>
    <button class="menu-item loc-toggle-row" role="switch" aria-checked="` + boolStr(remoteOnly) + `" onclick="setRemotePref(` + notBoolStr(remoteOnly) + `)">
      <span>Remote only</span>
      <span class="loc-toggle` + onIf(remoteOnly) + `" aria-hidden="true"></span>
    </button>`

	unlistedToggle := `<button class="menu-item loc-toggle-row loc-unlisted-row" role="switch" aria-checked="` + boolStr(includeUnknown) + `" onclick="setLocationPref('` + escapeHTML(selectedKey) + `', ` + notBoolStr(includeUnknown) + `)">
      <span>Include unlisted</span>
      <span class="loc-toggle` + onIf(includeUnknown) + `" aria-hidden="true"></span>
    </button>`

	return `<div class="dd-wrap">
    <button class="loc-trigger" id="loc-panel-btn" onclick="toggleLocPanel()" title="Location">
      <span>` + triggerLabel + `</span>
      <svg xmlns="http://www.w3.org/2000/svg" width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="6 9 12 15 18 9"/></svg>
    </button>
    <div id="loc-panel" class="loc-dropdown">
      ` + rows.String() + `
      ` + remoteToggle + `
      ` + unlistedToggle + `
    </div>
  </div>`
}


func renderSortDropdown(filter, sortKey string, opts SearchOptions) string {
	isDate := sortKey == "date"
	label := "Sort: Score"
	nextSort := "date"
	if isDate {
		label = "Sort: Date"
		nextSort = "score"
	}
	href := buildDashboardHref(filter, nextSort, opts)
	return `<a class="loc-trigger sort-toggle" href="` + href + `" title="Sort">` + label + `</a>`
}

// renderFilters ports renderFilters. metros nil/empty omits the location dropdown.
func renderFilters(filter, sortKey string, opts SearchOptions, prefs LocationPrefs, metros []metroOption) string {
	if filter == "analytics" {
		return `<div class="content-filters" id="dashboard-filters"></div>`
	}
	sortHTML := ""
	if filter != "market-research" {
		sortHTML = renderSortDropdown(filter, sortKey, opts)
	}
	return "\n<div class=\"content-filters\" id=\"dashboard-filters\">\n  " +
		renderLocationDropdown(prefs, metros) + "\n  " + sortHTML + "\n</div>"
}

// --- small helpers matching the JS ternaries ---

func activeIf(b bool) string {
	if b {
		return " active"
	}
	return ""
}

func onIf(b bool) string {
	if b {
		return " on"
	}
	return ""
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func notBoolStr(b bool) string { return boolStr(!b) }

func metroLabel(metros []metroOption, key string) string {
	for _, m := range metros {
		if m.Key == key {
			return m.Label
		}
	}
	return key
}

// --- renderDashboardTitle (from lib/dashboard-html.js) ---

type titleDef struct {
	label string
	key   string // "" means no count line
	noun  string
}

var titleDefs = map[string]titleDef{
	"all":             {"All Jobs", "total", "total"},
	"not-applied":     {"Jobs", "notApplied", "pending"},
	"applied":         {"Applications", "applied", "applied"},
	"interviewing":    {"Interviews", "interviewing", "interviewing"},
	"offers":          {"Offers", "offers", "offers"},
	"archived":        {"Archive", "archived", "archived"},
	"rejected":        {"Rejected", "rejected", "rejected"},
	"closed":          {"Closed", "closed", "closed"},
	"ghosted":         {"Ghosted", "ghosted", "ghosted"},
	"analytics":       {"Analytics", "", ""},
	"activity-log":    {"Event Log", "", ""},
	"market-research": {"Market Research", "", ""},
}

func statByKey(s db.Stats, key string) int {
	switch key {
	case "total":
		return s.Total
	case "notApplied":
		return s.NotApplied
	case "applied":
		return s.Applied
	case "interviewing":
		return s.Interviewing
	case "offers":
		return s.Offers
	case "archived":
		return s.Archived
	case "rejected":
		return s.Rejected
	case "closed":
		return s.Closed
	case "ghosted":
		return s.Ghosted
	}
	return 0
}

// renderPageStatusLine joins segments into the standardized one-line page
// status that sits under every page title: counts · last scrape Xm ago · N new.
// actionsHTML (optional) is appended right-aligned inside the same line.
func renderPageStatusLine(segs []string, actionsHTML string) string {
	if len(segs) == 0 && actionsHTML == "" {
		return ""
	}
	actions := ""
	if actionsHTML != "" {
		actions = `<span class="page-status-actions">` + actionsHTML + `</span>`
	}
	return `<div class="content-sub page-status">` + strings.Join(segs, " · ") + actions + `</div>`
}

// heartbeatSegs returns the healthy-heartbeat status segments ("last scrape Xm
// ago", "N new"). Error/stale states stay in the warning banner instead.
func heartbeatSegs(hb *db.Heartbeat) []string {
	if hb == nil || hb.Status == "error" || hoursSince(hb.LastSuccessAt) > scrapeStaleHours {
		return nil
	}
	ago := timeAgo(hb.LastSuccessAt)
	if ago == "" {
		return nil
	}
	segs := []string{"last scrape " + ago}
	if hb.Inserted != 0 {
		segs = append(segs, itoa(hb.Inserted)+" new this scrape")
	}
	return segs
}

// renderDashboardTitle ports renderDashboardTitle, folding the healthy scraper
// heartbeat into the one-line page status (shared page grammar).
func renderDashboardTitle(filter string, stats db.Stats, hb *db.Heartbeat) string {
	def, ok := titleDefs[filter]
	if !ok {
		def = titleDefs["all"]
	}
	var segs []string
	if filter == "all" {
		for _, seg := range []struct {
			n     int
			label string
		}{
			{stats.Total, "total"},
			{stats.NotApplied, "pending"},
			{stats.Applied, "applied"},
			{stats.Interviewing, "interviewing"},
		} {
			if seg.n != 0 {
				segs = append(segs, itoa(seg.n)+" "+seg.label)
			}
		}
	} else if def.key != "" {
		segs = append(segs, itoa(statByKey(stats, def.key))+" "+def.noun)
	}
	// Scrape freshness belongs to the job inbox views only; on Applications,
	// Interviews, Offers, and the report pages it is noise.
	if filter == "not-applied" || filter == "all" {
		segs = append(segs, heartbeatSegs(hb)...)
	}
	return `<h1 class="content-title">` + def.label + `</h1>` + renderPageStatusLine(segs, "")
}
