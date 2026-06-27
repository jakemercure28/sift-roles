package db

import (
	"regexp"
	"strconv"
	"strings"
)

// DBType is the storage backend a Repository is bound to.
type DBType string

const (
	// SQLite is the zero-config self-host backend (the default).
	SQLite DBType = "sqlite"
	// Postgres is the hosted multi-tenant backend (Supabase).
	Postgres DBType = "postgres"
)

// dialect rewrites SQLite-dialect SQL to the target backend at execution time.
//
// The whole codebase writes queries once, in SQLite dialect. On the SQLite
// backend rewrite is the identity function, so behavior is byte-for-byte
// unchanged and the existing tests exercise the literal queries. On Postgres,
// rewrite() translates the handful of SQLite-isms (datetime/date functions,
// julianday, INSERT OR IGNORE) and rebinds ?-placeholders to $N. This is the one
// place dialect divergence lives — see the migration tracks in migrations/ for
// the matching DDL.
type dialect struct {
	kind DBType
	// tz is the IANA timezone used to resolve SQLite's 'localtime' modifier on
	// Postgres. Empty means "backend default" (UTC on Supabase) and keeps the
	// rewrite byte-for-byte identical to the pre-timezone behavior. It is only ever
	// set to a time.LoadLocation-validated name (see config.getenvTimezone), so it
	// is safe to interpolate into the SQL the rewriter emits.
	tz string
}

// Postgres rewrite rules, ordered most-specific first. All timestamp expressions
// render to the same TEXT format SQLite produces ('YYYY-MM-DD HH:MM:SS', UTC), so
// the Go layer's string-based timestamp handling (lexicographic ordering, RFC3339
// parsing) is unchanged and timestamp columns stay TEXT on both backends.
var (
	// datetime('now', '-' || ? || ' days') — interval built from a bound int.
	reDatetimeDaysParam = regexp.MustCompile(`datetime\(\s*'now'\s*,\s*'-'\s*\|\|\s*\?\s*\|\|\s*' days'\s*\)`)
	// datetime('now', '-24 hours') — literal relative interval.
	reDatetimeLiteral = regexp.MustCompile(`datetime\(\s*'now'\s*,\s*'-(\d+)\s+([a-zA-Z]+)'\s*\)`)
	// datetime('now', ?) — modifier supplied as a bound string ("-30 days").
	reDatetimeParam = regexp.MustCompile(`datetime\(\s*'now'\s*,\s*\?\s*\)`)
	// datetime('now') — current UTC timestamp.
	reDatetimeNow = regexp.MustCompile(`datetime\(\s*'now'\s*\)`)
	// date('now','localtime') — current local date as 'YYYY-MM-DD'.
	reDateLocaltime = regexp.MustCompile(`date\(\s*'now'\s*,\s*'localtime'\s*\)`)
	// date(<column>,'localtime') — the stored timestamp's date as 'YYYY-MM-DD'.
	// The 'now' form above is matched first; this one only matches a column
	// identifier (never the quoted 'now' literal), so the two don't overlap.
	reDateColLocaltime = regexp.MustCompile(`date\(\s*([A-Za-z_][\w.]*)\s*,\s*'localtime'\s*\)`)
	// julianday('now') and julianday(<column>) — used only inside day-difference
	// expressions, so converting each term to epoch-days keeps the subtraction
	// correct.
	reJulianNow = regexp.MustCompile(`julianday\(\s*'now'\s*\)`)
	reJulianCol = regexp.MustCompile(`julianday\(\s*([A-Za-z_][\w.]*)\s*\)`)
)

const pgNow = `to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS')`

// julianColCast guards the timestamptz cast behind a date-shape check. Some
// columns (notably jobs.posted_at) hold scraper-sourced strings like
// "Posted 6 Days Ago" rather than a timestamp; casting those to timestamptz
// aborts the whole statement (SQLSTATE 22007). The CASE yields NULL for
// non-date text, and the surrounding day-difference arithmetic already
// tolerates NULL (it just drops out of the aggregate), so a single junk row
// no longer fails the query. ${1} is the column identifier captured by
// reJulianCol; it appears twice, so the cast only runs on date-shaped values.
const julianColCast = `(extract(epoch from (CASE WHEN ${1} ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}' THEN ${1} ELSE NULL END)::timestamptz) / 86400.0)`

// rewrite returns query unchanged on SQLite; on Postgres it translates the
// SQLite-isms and rebinds placeholders.
func (d dialect) rewrite(query string) string {
	if d.kind != Postgres {
		return query
	}

	query = reDatetimeDaysParam.ReplaceAllString(query,
		`to_char((now() - make_interval(days => (?)::int)) AT TIME ZONE 'utc', 'YYYY-MM-DD HH24:MI:SS')`)
	query = reDatetimeLiteral.ReplaceAllString(query,
		`to_char((now() - interval '$1 $2') AT TIME ZONE 'utc', 'YYYY-MM-DD HH24:MI:SS')`)
	query = reDatetimeParam.ReplaceAllString(query,
		`to_char((now() + (?)::interval) AT TIME ZONE 'utc', 'YYYY-MM-DD HH24:MI:SS')`)
	query = reDatetimeNow.ReplaceAllString(query, pgNow)
	if d.tz != "" {
		// SQLite's date(...,'localtime') means "the calendar date in the local
		// zone". now() is already timestamptz, so shift it into tz before taking
		// the date. Stored timestamps are UTC-naive TEXT, so the column form must
		// first be pinned to UTC, then converted to tz, before the date is taken.
		query = reDateLocaltime.ReplaceAllString(query,
			`to_char(now() AT TIME ZONE '`+d.tz+`', 'YYYY-MM-DD')`)
		query = reDateColLocaltime.ReplaceAllString(query,
			`to_char((($1)::timestamp AT TIME ZONE 'UTC') AT TIME ZONE '`+d.tz+`', 'YYYY-MM-DD')`)
	} else {
		query = reDateLocaltime.ReplaceAllString(query, `to_char(now(), 'YYYY-MM-DD')`)
		query = reDateColLocaltime.ReplaceAllString(query, `to_char(($1)::timestamp, 'YYYY-MM-DD')`)
	}
	query = reJulianNow.ReplaceAllString(query, `(extract(epoch from now()) / 86400.0)`)
	query = reJulianCol.ReplaceAllString(query, julianColCast)

	// INSERT OR IGNORE -> INSERT ... ON CONFLICT DO NOTHING. The unqualified
	// conflict target matches OR IGNORE's "swallow any uniqueness violation".
	if strings.Contains(query, "INSERT OR IGNORE") {
		query = strings.Replace(query, "INSERT OR IGNORE", "INSERT", 1)
		query = strings.TrimRight(query, " \t\n") + " ON CONFLICT DO NOTHING"
	}

	return rebind(query)
}

// rebind converts ?-placeholders to Postgres $N positional placeholders, left to
// right, skipping any ? inside single-quoted string literals.
func rebind(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	inQuote := false
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'':
			inQuote = !inQuote
			b.WriteByte(c)
		case c == '?' && !inQuote:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
