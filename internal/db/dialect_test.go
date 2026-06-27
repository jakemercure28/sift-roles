package db

import "testing"

// TestRewriteSQLiteIsIdentity is the load-bearing guarantee for the self-host
// backend: on SQLite the rewriter must never alter a query, so existing behavior
// and the rest of the test suite are unaffected.
func TestRewriteSQLiteIsIdentity(t *testing.T) {
	queries := []string{
		"SELECT value FROM metadata WHERE key = ?",
		"INSERT OR IGNORE INTO jobs (id) VALUES (?)",
		"UPDATE jobs SET updated_at = datetime('now') WHERE id = ?",
		"SELECT date('now','localtime')",
		"SELECT julianday(a) - julianday(b)",
	}
	// A configured timezone must never leak into the SQLite path: 'localtime' there
	// already resolves against the host zone, so the rewrite stays the identity even
	// when tz is set.
	for _, d := range []dialect{{kind: SQLite}, {kind: SQLite, tz: "America/Los_Angeles"}} {
		for _, q := range queries {
			if got := d.rewrite(q); got != q {
				t.Errorf("SQLite rewrite mutated query (tz=%q):\n in:  %s\n out: %s", d.tz, q, got)
			}
		}
	}
}

// TestRewritePostgresTimezone proves that a configured tz shifts both 'localtime'
// forms into that zone (the Gemini quota-window fix), while an empty tz preserves
// the pre-fix UTC-ish output. The day boundary is the only thing that changes.
func TestRewritePostgresTimezone(t *testing.T) {
	d := dialect{kind: Postgres, tz: "America/Los_Angeles"}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "local date of now shifts into tz",
			in:   "SELECT * FROM api_usage WHERE date = date('now','localtime') AND model = ?",
			want: "SELECT * FROM api_usage WHERE date = to_char(now() AT TIME ZONE 'America/Los_Angeles', 'YYYY-MM-DD') AND model = $1",
		},
		{
			name: "local date of a UTC-naive column converts UTC->tz",
			in:   "SELECT id FROM jobs WHERE date(created_at, 'localtime') = ? AND user_id = ?",
			want: "SELECT id FROM jobs WHERE to_char(((created_at)::timestamp AT TIME ZONE 'UTC') AT TIME ZONE 'America/Los_Angeles', 'YYYY-MM-DD') = $1 AND user_id = $2",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := d.rewrite(c.in); got != c.want {
				t.Errorf("rewrite mismatch:\n in:   %s\n want: %s\n got:  %s", c.in, c.want, got)
			}
		})
	}
}

func TestRewritePostgres(t *testing.T) {
	d := dialect{kind: Postgres}
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "placeholders rebind positionally",
			in:   "UPDATE jobs SET score=?, reasoning=? WHERE id=?",
			want: "UPDATE jobs SET score=$1, reasoning=$2 WHERE id=$3",
		},
		{
			name: "datetime now becomes to_char",
			in:   "UPDATE jobs SET updated_at = datetime('now') WHERE id = ?",
			want: "UPDATE jobs SET updated_at = to_char((now() AT TIME ZONE 'utc'), 'YYYY-MM-DD HH24:MI:SS') WHERE id = $1",
		},
		{
			name: "literal relative interval",
			in:   "SELECT 1 WHERE created_at >= datetime('now','-24 hours')",
			want: "SELECT 1 WHERE created_at >= to_char((now() - interval '24 hours') AT TIME ZONE 'utc', 'YYYY-MM-DD HH24:MI:SS')",
		},
		{
			name: "param days interval keeps single placeholder",
			in:   "SELECT id FROM jobs WHERE applied_at < datetime('now', '-' || ? || ' days')",
			want: "SELECT id FROM jobs WHERE applied_at < to_char((now() - make_interval(days => ($1)::int)) AT TIME ZONE 'utc', 'YYYY-MM-DD HH24:MI:SS')",
		},
		{
			name: "param modifier interval",
			in:   "SELECT COUNT(*) FROM rejection_email_log WHERE created_at >= datetime('now', ?)",
			want: "SELECT COUNT(*) FROM rejection_email_log WHERE created_at >= to_char((now() + ($1)::interval) AT TIME ZONE 'utc', 'YYYY-MM-DD HH24:MI:SS')",
		},
		{
			name: "local date",
			in:   "SELECT * FROM api_usage WHERE date = date('now','localtime') AND model = ?",
			want: "SELECT * FROM api_usage WHERE date = to_char(now(), 'YYYY-MM-DD') AND model = $1",
		},
		{
			name: "local date of a column (descriptions check)",
			in:   "SELECT id FROM jobs WHERE date(created_at, 'localtime') = ? AND user_id = ?",
			want: "SELECT id FROM jobs WHERE to_char((created_at)::timestamp, 'YYYY-MM-DD') = $1 AND user_id = $2",
		},
		{
			name: "julianday difference guards the timestamptz cast",
			in:   "SELECT julianday(e.created_at) - julianday(j.applied_at)",
			want: "SELECT (extract(epoch from (CASE WHEN e.created_at ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}' THEN e.created_at ELSE NULL END)::timestamptz) / 86400.0) - (extract(epoch from (CASE WHEN j.applied_at ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}' THEN j.applied_at ELSE NULL END)::timestamptz) / 86400.0)",
		},
		{
			name: "julianday now (column term still guarded)",
			in:   "SELECT julianday('now') - julianday(applied_at)",
			want: "SELECT (extract(epoch from now()) / 86400.0) - (extract(epoch from (CASE WHEN applied_at ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}' THEN applied_at ELSE NULL END)::timestamptz) / 86400.0)",
		},
		{
			name: "julianday on a column holding non-date text still rewrites (cast guarded at runtime)",
			in:   "SELECT julianday(j.posted_at) FROM jobs j",
			want: "SELECT (extract(epoch from (CASE WHEN j.posted_at ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}' THEN j.posted_at ELSE NULL END)::timestamptz) / 86400.0) FROM jobs j",
		},
		{
			name: "insert or ignore",
			in:   "INSERT OR IGNORE INTO jobs (id, title) VALUES (?, ?)",
			want: "INSERT INTO jobs (id, title) VALUES ($1, $2) ON CONFLICT DO NOTHING",
		},
		{
			name: "question mark inside string literal is not rebound",
			in:   "SELECT id FROM jobs WHERE status = 'pending' AND note != '?' AND id = ?",
			want: "SELECT id FROM jobs WHERE status = 'pending' AND note != '?' AND id = $1",
		},
		{
			name: "empty string literal does not break rebind",
			in:   "SELECT id FROM jobs WHERE COALESCE(stage, '') = '' AND id = ?",
			want: "SELECT id FROM jobs WHERE COALESCE(stage, '') = '' AND id = $1",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := d.rewrite(c.in); got != c.want {
				t.Errorf("rewrite mismatch:\n in:   %s\n want: %s\n got:  %s", c.in, c.want, got)
			}
		})
	}
}
