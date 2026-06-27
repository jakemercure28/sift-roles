package dashboard

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// htmlEscaper mirrors escapeHtml in lib/utils.js: &, <, >, ", ' in that order.
var htmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&#39;",
)

func escapeHTML(s string) string { return htmlEscaper.Replace(s) }

// Job is the row data the table renders (a subset of a jobs row).
type Job struct {
	ID                string
	Title             string
	Company           string
	URL               string
	Platform          string
	Location          string
	PostedAt          string
	CreatedAt         string
	UpdatedAt         string
	Description       string
	Score             *int
	Reasoning         string
	RejectionReason   string
	Status            string
	Stage             string
	AppliedAt         string
	RejectedFromStage string
	RejectedAt        string
}

// SearchOptions carries the dashboard list query state (q, minScore, page).
type SearchOptions struct {
	Q             string
	MinScore      int
	Page          int
	AnalysisError string
}

// Pagination is the paginate result passed to the table renderer.
type Pagination struct {
	Page       int
	PageSize   int
	TotalItems int
	TotalPages int
	StartItem  int
	EndItem    int
}

var isoDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)
var relDaysRe = regexp.MustCompile(`(?i)(\d+)\+?\s*day`)
var todayRe = regexp.MustCompile(`(?i)today`)

// postedTimestamp mirrors helpers.postedTimestamp: ISO dates become epoch millis,
// "today"/"N days" are relative to now (non-deterministic; fixtures avoid them).
func postedTimestamp(val string) int64 {
	if val == "" {
		return 0
	}
	if isoDateRe.MatchString(val) {
		if t, err := time.Parse("2006-01-02", val[:10]); err == nil {
			return t.UnixMilli()
		}
		return 0
	}
	if todayRe.MatchString(val) {
		return time.Now().UnixMilli()
	}
	if m := relDaysRe.FindStringSubmatch(val); m != nil {
		n, _ := strconv.Atoi(m[1])
		return time.Now().AddDate(0, 0, -n).UnixMilli()
	}
	return 0
}

// formatPosted mirrors helpers.formatPosted.
func formatPosted(val string) string {
	if val == "" {
		return "—"
	}
	if isoDateRe.MatchString(val) {
		return val[:10]
	}
	if todayRe.MatchString(val) {
		return time.Now().UTC().Format("2006-01-02")
	}
	if m := relDaysRe.FindStringSubmatch(val); m != nil {
		n, _ := strconv.Atoi(m[1])
		return time.Now().UTC().AddDate(0, 0, -n).Format("2006-01-02")
	}
	return "—"
}

// scoreClass mirrors helpers.scoreClass.
func scoreClass(score *int) string {
	if score == nil {
		return "score-null"
	}
	s := *score
	switch {
	case s >= 10: // >= 9.5
		return "score-10"
	case s >= 9: // >= 8.5
		return "score-9"
	case s >= 8: // >= 7.5
		return "score-8"
	case s >= 5:
		return "score-mid"
	default:
		return "score-low"
	}
}

// scoreColorVar mirrors theme.scoreColor: CSS-var references, not literals.
func scoreColorVar(score *int) string {
	if score == nil {
		return "transparent"
	}
	switch {
	case *score >= 7:
		return "var(--score-good)"
	case *score >= 5:
		return "var(--score-borderline)"
	default:
		return "var(--score-weak)"
	}
}

var pipelineLabels = map[string]string{
	"":             "—",
	"applied":      "Applied",
	"phone_screen": "Phone Screen",
	"interview":    "Interview",
	"onsite":       "Onsite",
	"offer":        "Offer",
	"closed":       "Closed",
	"rejected":     "Rejected",
	"ghosted":      "Ghosted",
}

// pipelineValue mirrors helpers.pipelineValue.
func pipelineValue(j Job) string {
	switch j.Stage {
	case "closed":
		return "closed"
	case "rejected":
		return "rejected"
	case "ghosted":
		return "ghosted"
	}
	if j.Status != "applied" && j.Status != "responded" {
		return ""
	}
	if j.Stage != "" {
		return j.Stage
	}
	return "applied"
}

func pipelineColor(val string) string {
	if c, ok := pipelineColors[val]; ok {
		return c
	}
	return pipelineColors[""]
}

var multiSpaceRe = regexp.MustCompile(`\s+`)

// normalizeViewOptions mirrors normalizeDashboardViewOptions in dashboard-search.js.
func normalizeViewOptions(o SearchOptions) SearchOptions {
	q := multiSpaceRe.ReplaceAllString(strings.TrimSpace(o.Q), " ")
	minScore := o.MinScore
	if minScore < 1 {
		minScore = 1
	}
	if minScore > 9 {
		minScore = 9
	}
	page := o.Page
	if page < 1 {
		page = 1
	}
	return SearchOptions{Q: q, MinScore: minScore, Page: page}
}

// buildDashboardHref mirrors helpers.buildDashboardHref. URLSearchParams preserves
// insertion order (filter, sort, q, minScore, page), so the query is built manually
// rather than via url.Values (which sorts).
func buildDashboardHref(filter, sort string, o SearchOptions) string {
	if filter == "" {
		filter = "all"
	}
	if sort == "" {
		sort = "score"
	}
	n := normalizeViewOptions(o)

	parts := []string{
		"filter=" + url.QueryEscape(filter),
		"sort=" + url.QueryEscape(sort),
	}
	if n.Q != "" {
		parts = append(parts, "q="+url.QueryEscape(n.Q))
	}
	if n.MinScore > 1 {
		parts = append(parts, "minScore="+strconv.Itoa(n.MinScore))
	}
	if n.Page > 1 {
		parts = append(parts, "page="+strconv.Itoa(n.Page))
	}
	return "/?" + strings.Join(parts, "&")
}
