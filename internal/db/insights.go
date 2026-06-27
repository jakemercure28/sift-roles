package db

import (
	"regexp"
	"strconv"
	"time"
)

// WeekCount is one weekly bucket for the Market Research "new roles per week"
// trend. JSON field names match the Node /api/market-activity response so the
// chart script (loadNewRoles) is unchanged.
type WeekCount struct {
	WeekStart string `json:"weekStart"`
	Label     string `json:"label"`
	Count     int    `json:"count"`
}

type weekRow struct {
	postedAt  string
	firstSeen string
	created   string
}

// NewRolesByWeek returns the count of new roles entering the market per week,
// bucketed by Monday, ported from getNewRolesByWeek in lib/dashboard-insights.js.
// period is one of 12w/26w/52w/all; callers validate it, but an unknown value
// falls back to 26 weeks to match the Node default.
func (r *Repository) NewRolesByWeek(period string) ([]WeekCount, error) {
	rows, err := r.query(
		`SELECT posted_at, first_seen_at, created_at FROM jobs
		 WHERE description IS NOT NULL AND length(description) > 100 AND user_id = ?`,
		r.userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var data []weekRow
	for rows.Next() {
		var posted, seen, created *string
		if err := rows.Scan(&posted, &seen, &created); err != nil {
			return nil, err
		}
		data = append(data, weekRow{
			postedAt:  derefStr(posted),
			firstSeen: derefStr(seen),
			created:   derefStr(created),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return computeNewRolesByWeek(data, period, time.Now().UTC()), nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// lookbackWeeks maps a period to its window length; 0 means "all weeks seen".
// Anything unrecognized is 26, matching the Node default.
func lookbackWeeks(period string) int {
	switch period {
	case "12w":
		return 12
	case "52w":
		return 52
	case "all":
		return 0
	default: // "26w" and any unknown value
		return 26
	}
}

// computeNewRolesByWeek is the pure core of NewRolesByWeek, taking `now` so the
// week window is testable. The container runs UTC (no TZ is set), so the Node
// "local" week math (Monday 00:00, en-US labels) is reproduced in UTC here.
func computeNewRolesByWeek(rows []weekRow, period string, now time.Time) []WeekCount {
	weeks := lookbackWeeks(period)

	counts := map[int64]int{} // week-start unix -> count
	for _, row := range rows {
		ts, ok := entryTimestamp(row, now)
		if !ok {
			continue
		}
		counts[weekStartOf(ts).Unix()]++
	}

	out := []WeekCount{}
	if len(counts) == 0 {
		return out
	}

	thisWeek := weekStartOf(now)
	var startWeek time.Time
	if weeks > 0 {
		startWeek = thisWeek.AddDate(0, 0, -7*(weeks-1))
	} else {
		min := int64(1<<63 - 1)
		for k := range counts {
			if k < min {
				min = k
			}
		}
		startWeek = time.Unix(min, 0).UTC()
	}

	for cursor := startWeek; !cursor.After(thisWeek); cursor = cursor.AddDate(0, 0, 7) {
		out = append(out, WeekCount{
			WeekStart: cursor.Format("2006-01-02"),
			Label:     cursor.Format("Jan 2"),
			Count:     counts[cursor.Unix()],
		})
	}
	return out
}

// weekStartOf returns Monday 00:00 (UTC) of the week containing t.
func weekStartOf(t time.Time) time.Time {
	t = t.UTC()
	y, m, d := t.Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	dow := (int(midnight.Weekday()) + 6) % 7 // Monday = 0
	return midnight.AddDate(0, 0, -dow)
}

// entryTimestamp resolves a row to a single entry instant, mirroring entryTs in
// lib/dashboard-insights.js: posted_at first, then first_seen_at, then
// created_at (the latter two stored as UTC SQLite datetimes).
func entryTimestamp(row weekRow, now time.Time) (time.Time, bool) {
	if t, ok := postedTimestamp(row.postedAt, now); ok {
		return t, true
	}
	if t, ok := parseSQLiteUTC(row.firstSeen); ok {
		return t, true
	}
	if t, ok := parseSQLiteUTC(row.created); ok {
		return t, true
	}
	return time.Time{}, false
}

var datePrefixRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)
var todayRe = regexp.MustCompile(`(?i)today`)
var nDaysRe = regexp.MustCompile(`(?i)(\d+)\+?\s*day`)

// postedTimestamp mirrors postedTimestamp in lib/html/helpers.js: an ISO date
// prefix resolves to that instant (UTC), "today" to now, and "N days" / "N+
// days" to N days before now. Anything else is not a timestamp.
func postedTimestamp(val string, now time.Time) (time.Time, bool) {
	if val == "" {
		return time.Time{}, false
	}
	if datePrefixRe.MatchString(val) {
		return parseJSDate(val)
	}
	if todayRe.MatchString(val) {
		return now, true
	}
	if m := nDaysRe.FindStringSubmatch(val); m != nil {
		n, _ := strconv.Atoi(m[1])
		return now.AddDate(0, 0, -n), true
	}
	return time.Time{}, false
}

// parseJSDate reproduces `new Date(val)` for ISO-date-prefixed strings: a bare
// "YYYY-MM-DD" is UTC midnight; a datetime without a zone is read as UTC (the
// container's local zone). Falls back to the leading date on partial parses.
func parseJSDate(val string) (time.Time, bool) {
	if len(val) == 10 {
		if t, err := time.ParseInLocation("2006-01-02", val, time.UTC); err == nil {
			return t, true
		}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, val, time.UTC); err == nil {
			return t.UTC(), true
		}
	}
	if len(val) >= 10 {
		if t, err := time.ParseInLocation("2006-01-02", val[:10], time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseSQLiteUTC parses a "YYYY-MM-DD HH:MM:SS" SQLite datetime as UTC, matching
// the Node `Date.parse(value + 'Z')` for first_seen_at/created_at.
func parseSQLiteUTC(val string) (time.Time, bool) {
	if val == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, val, time.UTC); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
