package db

import (
	"database/sql"
	"testing"
)

// ss/si build the NULL-able column values used when seeding test rows.
func ss(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
func si(n int) sql.NullInt64     { return sql.NullInt64{Int64: int64(n), Valid: true} }

// seedAltJob inserts a jobs row with the state columns the canonicalize merge
// reads. Empty strings seed as NULL (so blank/COALESCE behavior is exercised);
// empty created_at/first_seen_at default to now.
func seedAltJob(t *testing.T, r *Repository, j Job) {
	t.Helper()
	_, err := r.db.Exec(`
		INSERT INTO jobs (
			id, title, company, url, platform, location, posted_at, scraped_at, description,
			score, status, stage, notes, applied_at, first_seen_at, created_at
		) VALUES (
			?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''),
			COALESCE(NULLIF(?, ''), datetime('now')), COALESCE(NULLIF(?, ''), datetime('now'))
		)`,
		j.ID, strOrEmpty(j.Title), strOrEmpty(j.Company), strOrEmpty(j.URL),
		strOrEmpty(j.Platform), strOrEmpty(j.Location), strOrEmpty(j.PostedAt),
		strOrEmpty(j.ScrapedAt), strOrEmpty(j.Description),
		intOrNil(j.Score), strOrEmpty(j.Status), strOrEmpty(j.Stage), strOrEmpty(j.Notes),
		strOrEmpty(j.AppliedAt), strOrEmpty(j.FirstSeenAt), strOrEmpty(j.CreatedAt),
	)
	if err != nil {
		t.Fatalf("seed %s: %v", j.ID, err)
	}
}

func mustGetJob(t *testing.T, r *Repository, id string) *Job {
	t.Helper()
	j, ok, err := r.getJobByID(r.db, id)
	if err != nil {
		t.Fatalf("getJobByID %s: %v", id, err)
	}
	if !ok {
		t.Fatalf("job %s not found", id)
	}
	return j
}

type aliasRow struct {
	canonicalID      string
	originalPlatform string
	resolvedPlatform string
	status           string
	confidence       float64
	confidenceValid  bool
}

func readAlias(t *testing.T, r *Repository, alternateID string) aliasRow {
	t.Helper()
	var canonical, origPlat, resPlat sql.NullString
	var conf sql.NullFloat64
	var status string
	err := r.db.QueryRow(`
		SELECT canonical_job_id, original_platform, resolved_platform, status, confidence
		FROM job_aliases WHERE alternate_job_id = ?`, alternateID).
		Scan(&canonical, &origPlat, &resPlat, &status, &conf)
	if err != nil {
		t.Fatalf("read alias %s: %v", alternateID, err)
	}
	return aliasRow{
		canonicalID:      canonical.String,
		originalPlatform: origPlat.String,
		resolvedPlatform: resPlat.String,
		status:           status,
		confidence:       conf.Float64,
		confidenceValid:  conf.Valid,
	}
}

func TestCanonicalizePrimaryNew(t *testing.T) {
	repo := newTestRepo(t)

	seedAltJob(t, repo, Job{
		ID: "alt1", Title: ss("Senior SRE"), Company: ss("Acme"),
		URL: ss("https://linkedin.com/jobs/123"), Platform: ss("linkedin"),
		Status: ss("applied"), Stage: ss("phone_screen"), Notes: ss("strong"),
		AppliedAt: ss("2026-06-01 00:00:00"), FirstSeenAt: ss("2026-05-20 00:00:00"),
		Score: si(7),
	})
	// An event on the alternate, to verify re-keying to the canonical id.
	if _, err := repo.db.Exec(
		`INSERT INTO events (job_id, event_type, to_value) VALUES ('alt1', 'scraped', 'linkedin')`,
	); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	alt := mustGetJob(t, repo, "alt1")
	res := Resolution{
		Status: "primary", Platform: "Greenhouse",
		URL: "https://boards.greenhouse.io/acme/jobs/999",
		Job: &ResolvedJob{
			ID: "gh999", Title: "Senior Site Reliability Engineer", Company: "Acme",
			URL: "https://boards.greenhouse.io/acme/jobs/999", Platform: "greenhouse",
			Location: "Remote", PostedAt: "2026-05-15", Description: "Run the fleet.",
		},
		Confidence: 0.95,
		Evidence:   map[string]any{"method": "direct-url"},
	}

	got, err := repo.CanonicalizeAlternateJob(alt, res)
	if err != nil {
		t.Fatalf("CanonicalizeAlternateJob: %v", err)
	}
	if got.Action != "canonicalized" || got.CanonicalID != "gh999" {
		t.Fatalf("result = %+v, want canonicalized/gh999", got)
	}

	// Canonical row inherited the alternate's pipeline state and content.
	canon := mustGetJob(t, repo, "gh999")
	if strOrEmpty(canon.Status) != "applied" {
		t.Errorf("canonical status = %q, want applied", strOrEmpty(canon.Status))
	}
	if strOrEmpty(canon.Stage) != "phone_screen" {
		t.Errorf("canonical stage = %q, want phone_screen", strOrEmpty(canon.Stage))
	}
	if !canon.Score.Valid || canon.Score.Int64 != 7 {
		t.Errorf("canonical score = %v, want 7", canon.Score)
	}
	if strOrEmpty(canon.FirstSeenAt) != "2026-05-20 00:00:00" {
		t.Errorf("canonical first_seen_at = %q, want 2026-05-20", strOrEmpty(canon.FirstSeenAt))
	}
	if strOrEmpty(canon.Title) != "Senior Site Reliability Engineer" {
		t.Errorf("canonical title = %q", strOrEmpty(canon.Title))
	}
	if strOrEmpty(canon.Location) != "Remote" {
		t.Errorf("canonical location = %q, want Remote", strOrEmpty(canon.Location))
	}

	// Alternate archived.
	if s := strOrEmpty(mustGetJob(t, repo, "alt1").Status); s != "archived" {
		t.Errorf("alternate status = %q, want archived", s)
	}

	// Alias recorded.
	a := readAlias(t, repo, "alt1")
	if a.status != "primary" || a.canonicalID != "gh999" {
		t.Errorf("alias = %+v, want primary/gh999", a)
	}
	if a.originalPlatform != "linkedin" || a.resolvedPlatform != "Greenhouse" {
		t.Errorf("alias platforms = %q/%q", a.originalPlatform, a.resolvedPlatform)
	}
	if !a.confidenceValid || a.confidence != 0.95 {
		t.Errorf("alias confidence = %v (valid=%v), want 0.95", a.confidence, a.confidenceValid)
	}

	// Event re-keyed from alt1 to gh999, plus an ats_resolution event on gh999.
	var scrapedJobID string
	if err := repo.db.QueryRow(
		`SELECT job_id FROM events WHERE event_type='scraped'`).Scan(&scrapedJobID); err != nil {
		t.Fatalf("read scraped event: %v", err)
	}
	if scrapedJobID != "gh999" {
		t.Errorf("scraped event job_id = %q, want gh999 (re-keyed)", scrapedJobID)
	}
	var from, to string
	if err := repo.db.QueryRow(
		`SELECT from_value, to_value FROM events WHERE event_type='ats_resolution'`).Scan(&from, &to); err != nil {
		t.Fatalf("read ats_resolution event: %v", err)
	}
	if from != "linkedin" || to != "Greenhouse" {
		t.Errorf("ats_resolution event = %q->%q, want linkedin->Greenhouse", from, to)
	}
}

func TestCanonicalizePrimaryMerge(t *testing.T) {
	repo := newTestRepo(t)

	// Canonical primary row already exists, pending, with blank state and an empty
	// location; the alternate carries richer state and an earlier first_seen_at.
	seedAltJob(t, repo, Job{
		ID: "gh999", Title: ss("Senior Site Reliability Engineer"), Company: ss("Acme"),
		URL: ss("https://boards.greenhouse.io/acme/jobs/999"), Platform: ss("greenhouse"),
		Status: ss("pending"), FirstSeenAt: ss("2026-06-10 00:00:00"),
	})
	seedAltJob(t, repo, Job{
		ID: "alt1", Title: ss("Senior SRE"), Company: ss("Acme"),
		URL: ss("https://linkedin.com/jobs/123"), Platform: ss("linkedin"),
		Location: ss("Remote"), Status: ss("applied"), Stage: ss("phone_screen"),
		Notes: ss("strong"), AppliedAt: ss("2026-06-01 00:00:00"),
		FirstSeenAt: ss("2026-05-20 00:00:00"), Score: si(7),
	})

	alt := mustGetJob(t, repo, "alt1")
	res := Resolution{
		Status: "primary", Platform: "Greenhouse",
		URL: "https://boards.greenhouse.io/acme/jobs/999",
		Job: &ResolvedJob{
			ID: "gh999", Title: "Senior Site Reliability Engineer", Company: "Acme",
			URL: "https://boards.greenhouse.io/acme/jobs/999", Platform: "greenhouse",
			Location: "Remote",
		},
		Confidence: 0.9,
	}

	if _, err := repo.CanonicalizeAlternateJob(alt, res); err != nil {
		t.Fatalf("CanonicalizeAlternateJob: %v", err)
	}

	canon := mustGetJob(t, repo, "gh999")
	if strOrEmpty(canon.Status) != "applied" { // pending escalates to applied
		t.Errorf("status = %q, want applied", strOrEmpty(canon.Status))
	}
	if strOrEmpty(canon.FirstSeenAt) != "2026-05-20 00:00:00" { // earlier date wins
		t.Errorf("first_seen_at = %q, want 2026-05-20", strOrEmpty(canon.FirstSeenAt))
	}
	if !canon.Score.Valid || canon.Score.Int64 != 7 { // backfilled from blank
		t.Errorf("score = %v, want 7", canon.Score)
	}
	if strOrEmpty(canon.Stage) != "phone_screen" {
		t.Errorf("stage = %q, want phone_screen", strOrEmpty(canon.Stage))
	}
	if strOrEmpty(canon.Notes) != "strong" {
		t.Errorf("notes = %q, want strong", strOrEmpty(canon.Notes))
	}
	if strOrEmpty(canon.Location) != "Remote" { // content backfill via COALESCE(NULLIF)
		t.Errorf("location = %q, want Remote", strOrEmpty(canon.Location))
	}
}

func TestCanonicalizeUnsupported(t *testing.T) {
	repo := newTestRepo(t)
	seedAltJob(t, repo, Job{
		ID: "alt2", Title: ss("Remote DevOps"), Company: ss("Globex"),
		URL: ss("https://weworkremotely.com/jobs/abc"), Platform: ss("weworkremotely"),
		Status: ss("pending"),
	})

	alt := mustGetJob(t, repo, "alt2")
	res := Resolution{Status: "unsupported", Evidence: map[string]any{"reason": "unsupported-host"}}

	got, err := repo.CanonicalizeAlternateJob(alt, res)
	if err != nil {
		t.Fatalf("CanonicalizeAlternateJob: %v", err)
	}
	if got.Action != "unsupported" || got.CanonicalID != "" {
		t.Fatalf("result = %+v, want unsupported/empty", got)
	}

	if s := strOrEmpty(mustGetJob(t, repo, "alt2").Status); s != "archived" {
		t.Errorf("alt2 status = %q, want archived", s)
	}
	a := readAlias(t, repo, "alt2")
	if a.status != "unsupported" || a.canonicalID != "" {
		t.Errorf("alias = %+v, want unsupported/empty canonical", a)
	}
	if a.confidenceValid {
		t.Errorf("confidence should be NULL when 0, got %v", a.confidence)
	}
	var from, to string
	if err := repo.db.QueryRow(
		`SELECT from_value, to_value FROM events WHERE event_type='ats_resolution' AND job_id='alt2'`,
	).Scan(&from, &to); err != nil {
		t.Fatalf("read ats_resolution event: %v", err)
	}
	if from != "weworkremotely" || to != "unsupported" {
		t.Errorf("event = %q->%q, want weworkremotely->unsupported", from, to)
	}
}

func TestCanonicalizeUnresolvedKeepsRow(t *testing.T) {
	repo := newTestRepo(t)
	seedAltJob(t, repo, Job{
		ID: "alt3", Title: ss("Platform Engineer"), Company: ss("Initech"),
		URL: ss("https://example.com/jobs/xyz"), Platform: ss("other"), Status: ss("pending"),
	})

	alt := mustGetJob(t, repo, "alt3")
	got, err := repo.CanonicalizeAlternateJob(alt, Resolution{Status: "unresolved"})
	if err != nil {
		t.Fatalf("CanonicalizeAlternateJob: %v", err)
	}
	if got.Action != "unresolved" {
		t.Fatalf("action = %q, want unresolved", got.Action)
	}
	if s := strOrEmpty(mustGetJob(t, repo, "alt3").Status); s != "pending" {
		t.Errorf("alt3 status = %q, want pending (not archived)", s)
	}
	if readAlias(t, repo, "alt3").status != "unresolved" {
		t.Errorf("alias status not recorded as unresolved")
	}
	var n int
	if err := repo.db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE event_type='ats_resolution'`).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 0 {
		t.Errorf("ats_resolution events = %d, want 0 for unresolved", n)
	}
}

func TestCanonicalizeEmptyStatusDefaultsUnresolved(t *testing.T) {
	repo := newTestRepo(t)
	seedAltJob(t, repo, Job{ID: "alt4", Title: ss("X"), Company: ss("Y"), Status: ss("pending")})
	alt := mustGetJob(t, repo, "alt4")
	got, err := repo.CanonicalizeAlternateJob(alt, Resolution{}) // empty status
	if err != nil {
		t.Fatalf("CanonicalizeAlternateJob: %v", err)
	}
	if got.Action != "unresolved" {
		t.Errorf("action = %q, want unresolved (empty status default)", got.Action)
	}
	if readAlias(t, repo, "alt4").status != "unresolved" {
		t.Errorf("alias status = want unresolved")
	}
}

func TestCanonicalizeRequiresAlternateID(t *testing.T) {
	repo := newTestRepo(t)
	if _, err := repo.CanonicalizeAlternateJob(&Job{}, Resolution{Status: "primary"}); err == nil {
		t.Fatal("expected error for empty alternate id")
	}
	if _, err := repo.CanonicalizeAlternateJob(nil, Resolution{}); err == nil {
		t.Fatal("expected error for nil alternate")
	}
}

func TestSelectAlternateJobsOnlyPending(t *testing.T) {
	repo := newTestRepo(t)
	seedAltJob(t, repo, Job{
		ID: "pending-alt", Title: ss("Staff SRE"), Company: ss("Acme"),
		URL: ss("https://linkedin.com/jobs/1"), Platform: ss("linkedin"), Status: ss("pending"),
	})
	seedAltJob(t, repo, Job{
		ID: "applied-alt", Title: ss("Staff SRE"), Company: ss("Acme"),
		URL: ss("https://builtin.com/jobs/1"), Platform: ss("builtin"), Status: ss("applied"),
	})
	seedAltJob(t, repo, Job{
		ID: "primary", Title: ss("Staff SRE"), Company: ss("Acme"),
		URL: ss("https://boards.greenhouse.io/acme/jobs/1"), Platform: ss("greenhouse"), Status: ss("pending"),
	})

	rows, err := repo.SelectAlternateJobs(true)
	if err != nil {
		t.Fatalf("SelectAlternateJobs: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "pending-alt" {
		t.Fatalf("rows = %#v, want only pending-alt", rows)
	}

	rows, err = repo.SelectAlternateJobs(false)
	if err != nil {
		t.Fatalf("SelectAlternateJobs all: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != "pending-alt" || rows[1].ID != "applied-alt" {
		t.Fatalf("rows = %#v, want pending then applied alternates", rows)
	}
}

func TestPreferredStatus(t *testing.T) {
	cases := []struct{ current, alt, want string }{
		{"pending", "applied", "applied"},  // higher rank wins
		{"applied", "pending", "applied"},  // lower rank loses
		{"applied", "archived", "applied"}, // archived never overwrites
		{"", "", "pending"},                // empties default to pending
		{"rejected", "responded", "responded"},
		{"closed", "rejected", "rejected"},
		{"archived", "archived", "archived"}, // archived may stay archived
	}
	for _, c := range cases {
		if got := preferredStatus(c.current, c.alt); got != c.want {
			t.Errorf("preferredStatus(%q,%q) = %q, want %q", c.current, c.alt, got, c.want)
		}
	}
}

func TestEarliestDate(t *testing.T) {
	cases := []struct{ left, right, want string }{
		{"2026-06-10 00:00:00", "2026-05-20 00:00:00", "2026-05-20 00:00:00"},
		{"2026-05-20 00:00:00", "2026-06-10 00:00:00", "2026-05-20 00:00:00"},
		{"", "2026-05-20 00:00:00", "2026-05-20 00:00:00"},
		{"2026-05-20 00:00:00", "", "2026-05-20 00:00:00"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := earliestDate(c.left, c.right); got != c.want {
			t.Errorf("earliestDate(%q,%q) = %q, want %q", c.left, c.right, got, c.want)
		}
	}
}
