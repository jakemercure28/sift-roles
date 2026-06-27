package db

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// reISODatePrefix matches a value that already starts with a YYYY-MM-DD date
// (optionally followed by a time component). Anything matching is left as-is.
var reISODatePrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)

// reRelativeAgo pulls "<n> <unit> ago" out of a human string like
// "Posted 6 Days Ago" or "30+ days ago".
var reRelativeAgo = regexp.MustCompile(`(?i)(\d+)\+?\s*(hour|day|week|month|year)s?\s+ago`)

// normalizePostedAt converts a scraper-sourced posted_at string into an absolute
// YYYY-MM-DD date the Postgres timestamptz cast can parse. Boards frequently
// return relative phrases ("Posted 6 Days Ago", "yesterday") rather than a real
// date; left untouched, those abort day-difference queries on Postgres
// (SQLSTATE 22007 — see dialect.go's julianColCast guard, which is the runtime
// backstop). Normalizing on write keeps the stored data clean and the dashboard
// dates renderable. Unrecognized non-date text collapses to "" so it reads as
// "unknown" rather than masquerading as junk.
//
// now is injected for testability; callers pass time.Now().UTC().
func normalizePostedAt(raw string, now time.Time) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// Already a real date/timestamp — keep it verbatim.
	if reISODatePrefix.MatchString(s) {
		return s
	}

	lower := strings.ToLower(s)
	day := now.Truncate(24 * time.Hour)

	switch {
	case strings.Contains(lower, "yesterday"):
		return day.AddDate(0, 0, -1).Format("2006-01-02")
	case strings.Contains(lower, "today"),
		strings.Contains(lower, "just posted"),
		strings.Contains(lower, "just now"):
		return day.Format("2006-01-02")
	}

	if m := reRelativeAgo.FindStringSubmatch(lower); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil {
			switch m[2] {
			case "hour":
				return day.Format("2006-01-02")
			case "day":
				return day.AddDate(0, 0, -n).Format("2006-01-02")
			case "week":
				return day.AddDate(0, 0, -7*n).Format("2006-01-02")
			case "month":
				return day.AddDate(0, -n, 0).Format("2006-01-02")
			case "year":
				return day.AddDate(-n, 0, 0).Format("2006-01-02")
			}
		}
	}

	// Unrecognized, non-date text: drop it rather than store junk.
	return ""
}
