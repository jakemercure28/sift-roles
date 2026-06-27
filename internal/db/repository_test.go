package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"job-search-automation/internal/model"
)

// newTestRepo opens a fresh database; Open runs the goose baseline to build the
// full schema, so tests exercise the real migration path.
func newTestRepo(t *testing.T) *Repository {
	t.Helper()
	repo, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { repo.Close() })
	return repo
}

func mustExec(t *testing.T, conn *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(query, args...); err != nil {
		t.Fatalf("exec seed SQL: %v", err)
	}
}

func sampleLead(id string) model.Lead {
	return model.Lead{
		JobLead: model.JobLead{
			Title:            "Backend Engineer",
			Company:          "Lightning AI",
			Description:      "Build things.",
			DirectApplyURL:   "https://job-boards.greenhouse.io/lightningai/jobs/5740846003",
			ATSPlatformName:  "Greenhouse",
			ScrapedTimestamp: "2026-06-05T10:00:00.000Z",
			Location:         "Remote",
			PostedAt:         "2026-06-01",
		},
		ID: id,
	}
}

func TestInsertScrapedLeadIsIdempotent(t *testing.T) {
	repo := newTestRepo(t)
	lead := sampleLead("greenhouse-5740846003")

	inserted, err := repo.InsertScrapedLead(lead)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if !inserted {
		t.Fatal("first insert should report inserted=true")
	}

	inserted, err = repo.InsertScrapedLead(lead)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if inserted {
		t.Fatal("second insert of same id should report inserted=false")
	}

	var count int
	if err := repo.db.QueryRow("SELECT COUNT(*) FROM jobs WHERE id = ?", lead.ID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row, got %d", count)
	}

	// New scraped rows must be pending + unscored so the Node scorer picks them up.
	var status string
	var score sql.NullInt64
	if err := repo.db.QueryRow("SELECT status, score FROM jobs WHERE id = ?", lead.ID).Scan(&status, &score); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "pending" {
		t.Fatalf("status = %q, want pending", status)
	}
	if score.Valid {
		t.Fatalf("score should be NULL, got %d", score.Int64)
	}

	var globalID string
	if err := repo.db.QueryRow("SELECT global_job_id FROM jobs WHERE id = ?", lead.ID).Scan(&globalID); err != nil {
		t.Fatalf("read global_job_id: %v", err)
	}
	if globalID != lead.ID {
		t.Fatalf("global_job_id = %q, want %q", globalID, lead.ID)
	}
}

func TestInsertScrapedLeadHarvestsGlobalJob(t *testing.T) {
	repo := newTestRepo(t)
	lead := sampleLead("greenhouse-global")

	if _, err := repo.InsertScrapedLead(lead); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var title, description, firstSeenBy string
	if err := repo.db.QueryRow(
		"SELECT title, description, first_seen_by FROM global_jobs WHERE id = ?",
		lead.ID,
	).Scan(&title, &description, &firstSeenBy); err != nil {
		t.Fatalf("read global job: %v", err)
	}
	if title != lead.Title || description != lead.Description || firstSeenBy != LocalUser {
		t.Fatalf("global row title=%q description=%q first_seen_by=%q", title, description, firstSeenBy)
	}
}

func TestInsertScrapedLeadDoesNotHarvestManualImport(t *testing.T) {
	repo := newTestRepo(t)
	lead := sampleLead("manual-linkedin")
	lead.ATSPlatformName = "linkedin"

	if _, err := repo.InsertScrapedLead(lead); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var count int
	if err := repo.db.QueryRow("SELECT COUNT(*) FROM global_jobs WHERE id = ?", lead.ID).Scan(&count); err != nil {
		t.Fatalf("count global jobs: %v", err)
	}
	if count != 0 {
		t.Fatalf("manual import was harvested into global_jobs")
	}
}

func TestSeedTenantJobsFromGlobal(t *testing.T) {
	repo := newTestRepo(t)
	mustExec(t, repo.db, `
		INSERT INTO global_jobs
		  (id, title, company, url, platform, location, posted_at, scraped_at, description, description_hash, first_seen_by)
		VALUES
		  ('g1', 'Platform Engineer', 'Acme', 'https://example.com/g1', 'greenhouse', 'Remote', '2026-06-01', '2026-06-02T00:00:00Z', 'Build infra.', 'h1', 'tenant-a')`)

	inserted, err := repo.SeedTenantJobsFromGlobal(10)
	if err != nil {
		t.Fatalf("SeedTenantJobsFromGlobal: %v", err)
	}
	if inserted != 1 {
		t.Fatalf("inserted = %d, want 1", inserted)
	}
	inserted, err = repo.SeedTenantJobsFromGlobal(10)
	if err != nil {
		t.Fatalf("SeedTenantJobsFromGlobal second run: %v", err)
	}
	if inserted != 0 {
		t.Fatalf("second inserted = %d, want 0", inserted)
	}

	var globalID, status string
	if err := repo.db.QueryRow("SELECT global_job_id, status FROM jobs WHERE id = 'g1'").Scan(&globalID, &status); err != nil {
		t.Fatalf("read seeded job: %v", err)
	}
	if globalID != "g1" || status != "pending" {
		t.Fatalf("seeded global_id=%q status=%q", globalID, status)
	}
}

func TestJobRowIDScopesHostedTenant(t *testing.T) {
	repo := newTestRepo(t)
	if got := repo.JobRowID("greenhouse-1"); got != "greenhouse-1" {
		t.Fatalf("self-host row id = %q", got)
	}
	repo.dl.kind = Postgres
	repo.userID = "tenant-a"
	a := repo.JobRowID("greenhouse-1")
	b := repo.JobRowID("greenhouse-1")
	if a == "greenhouse-1" || a == "" {
		t.Fatalf("hosted tenant row id was not scoped: %q", a)
	}
	if a != b {
		t.Fatalf("row id should be deterministic: %q vs %q", a, b)
	}
}

// TestTenantsSelfHost guards the self-host invariant: on SQLite the background
// pipeline must see exactly one tenant (LocalUser) without touching the DB, so the
// per-tenant fan-out collapses to today's single pass.
func TestTenantsSelfHost(t *testing.T) {
	repo := newTestRepo(t)
	tenants, err := repo.Tenants()
	if err != nil {
		t.Fatalf("Tenants: %v", err)
	}
	if len(tenants) != 1 || tenants[0] != LocalUser {
		t.Fatalf("Tenants = %v, want [%q]", tenants, LocalUser)
	}
}

func TestInsertScrapedLeadPatchesEmptyLocation(t *testing.T) {
	repo := newTestRepo(t)

	noLoc := sampleLead("greenhouse-empty-loc")
	noLoc.Location = ""
	if _, err := repo.InsertScrapedLead(noLoc); err != nil {
		t.Fatalf("insert without location: %v", err)
	}

	withLoc := noLoc
	withLoc.Location = "Seattle, WA"
	inserted, err := repo.InsertScrapedLead(withLoc)
	if err != nil {
		t.Fatalf("re-insert with location: %v", err)
	}
	if inserted {
		t.Fatal("re-insert of existing id should report inserted=false")
	}

	var loc string
	if err := repo.db.QueryRow("SELECT location FROM jobs WHERE id = ?", withLoc.ID).Scan(&loc); err != nil {
		t.Fatalf("read location: %v", err)
	}
	if loc != "Seattle, WA" {
		t.Fatalf("location = %q, want backfilled value", loc)
	}
}

func TestLogEventNullsEmptyValues(t *testing.T) {
	repo := newTestRepo(t)
	lead := sampleLead("greenhouse-evt")
	if _, err := repo.InsertScrapedLead(lead); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := repo.LogEvent(lead.ID, "scraped", "", ""); err != nil {
		t.Fatalf("LogEvent: %v", err)
	}

	var eventType string
	var from, to sql.NullString
	if err := repo.db.QueryRow(
		"SELECT event_type, from_value, to_value FROM events WHERE job_id = ?", lead.ID,
	).Scan(&eventType, &from, &to); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if eventType != "scraped" {
		t.Fatalf("event_type = %q", eventType)
	}
	if from.Valid || to.Valid {
		t.Fatalf("empty from/to should be NULL, got from=%v to=%v", from, to)
	}
}

func TestRecentEventsOnlyReturnsUserFacingActivity(t *testing.T) {
	repo := newTestRepo(t)
	lead := sampleLead("greenhouse-visible-events")
	if _, err := repo.InsertScrapedLead(lead); err != nil {
		t.Fatalf("insert: %v", err)
	}

	mustExec(t, repo.db, `
		INSERT INTO events (user_id, job_id, event_type, from_value, to_value, created_at) VALUES
		('local', ?, 'scraped', NULL, 'Greenhouse', '2026-06-01 00:00:00'),
		('local', ?, 'ats_resolution', 'Built In', 'unsupported', '2026-06-02 00:00:00'),
		('local', ?, 'stage_change', 'applied', 'closed', '2026-06-03 00:00:00'),
		('local', ?, 'stage_change', 'applied', 'ghosted', '2026-06-04 00:00:00'),
		('local', ?, 'stage_change', 'interview', 'rejected', '2026-06-05 00:00:00'),
		('local', ?, 'status_change', NULL, 'archived', '2026-06-06 00:00:00'),
		('local', ?, 'outreach', NULL, 'reached_out', '2026-06-07 00:00:00'),
		('local', ?, 'auto_applied', NULL, 'applied', '2026-06-08 00:00:00')`,
		lead.ID, lead.ID, lead.ID, lead.ID, lead.ID, lead.ID, lead.ID, lead.ID,
	)

	events, err := repo.RecentEvents()
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(events) != 6 {
		t.Fatalf("RecentEvents returned %d rows, want 6: %+v", len(events), events)
	}
	for _, event := range events {
		if event.EventType == "scraped" || event.EventType == "ats_resolution" {
			t.Fatalf("internal event leaked into Activity Log feed: %+v", event)
		}
	}
	wantNewest := []string{"auto_applied", "outreach", "status_change", "stage_change", "stage_change", "stage_change"}
	for i, want := range wantNewest {
		if events[i].EventType != want {
			t.Fatalf("events[%d].EventType = %q, want %q", i, events[i].EventType, want)
		}
	}
}

func TestExistingJobKeys(t *testing.T) {
	repo := newTestRepo(t)
	lead := sampleLead("greenhouse-keys")
	if _, err := repo.InsertScrapedLead(lead); err != nil {
		t.Fatalf("insert: %v", err)
	}

	keys, err := repo.ExistingJobKeys()
	if err != nil {
		t.Fatalf("ExistingJobKeys: %v", err)
	}
	want := "backend engineer|||lightning ai"
	if _, ok := keys[want]; !ok {
		t.Fatalf("expected key %q in %v", want, keys)
	}
}

func TestActiveTenantSelfHostIsLocalUser(t *testing.T) {
	repo := newTestRepo(t)
	// Self-host SQLite is single-tenant: ActiveTenant returns LocalUser without
	// touching the DB, so the background pipeline keeps reading the root data dir
	// (TenantDataDir maps LocalUser back to the base path).
	uid, err := repo.ActiveTenant()
	if err != nil {
		t.Fatalf("ActiveTenant: %v", err)
	}
	if uid != LocalUser {
		t.Fatalf("ActiveTenant = %q, want %q", uid, LocalUser)
	}
}

func TestActiveTenantPostgresIgnoresLocalTenant(t *testing.T) {
	repo := newTestRepo(t)
	repo.dl.kind = Postgres
	mustExec(t, repo.db, `
		INSERT INTO jobs (user_id, id, title, company, url, status)
		VALUES
			('local', 'local-1', 'Local', 'Acme', 'https://example.com/local-1', 'pending'),
			('local', 'local-2', 'Local', 'Acme', 'https://example.com/local-2', 'pending'),
			('tenant-a', 'tenant-a-1', 'Tenant', 'Acme', 'https://example.com/tenant-a-1', 'pending')
	`)

	uid, err := repo.ActiveTenant()
	if err != nil {
		t.Fatalf("ActiveTenant: %v", err)
	}
	if uid != "tenant-a" {
		t.Fatalf("ActiveTenant = %q, want tenant-a", uid)
	}
}

func TestProfileDirPostgresUsesActiveTenant(t *testing.T) {
	repo := newTestRepo(t)
	repo.dl.kind = Postgres
	mustExec(t, repo.db, `
		INSERT INTO jobs (user_id, id, title, company, url, status)
		VALUES ('tenant-a', 'tenant-a-1', 'Tenant', 'Acme', 'https://example.com/tenant-a-1', 'pending')
	`)

	base := t.TempDir()
	want := filepath.Join(base, "storage", "users", "tenant-a")
	if got := repo.ProfileDir(base); got != want {
		t.Fatalf("ProfileDir = %q, want %q", got, want)
	}
}

func TestWriteHeartbeat(t *testing.T) {
	repo := newTestRepo(t)

	if err := repo.WriteHeartbeat("ok", 5, 2, ""); err != nil {
		t.Fatalf("write ok: %v", err)
	}
	hb := repo.readHeartbeat()
	if hb.Status != "ok" || hb.Scraped != 5 || hb.Inserted != 2 {
		t.Fatalf("unexpected heartbeat: %+v", hb)
	}
	if hb.LastSuccessAt == "" {
		t.Fatal("ok heartbeat should set last_success_at")
	}
	firstSuccess := hb.LastSuccessAt

	// An error must preserve the prior last_success_at and record the error.
	if err := repo.WriteHeartbeat("error", 0, 0, "boom"); err != nil {
		t.Fatalf("write error: %v", err)
	}
	hb = repo.readHeartbeat()
	if hb.Status != "error" || hb.Error != "boom" {
		t.Fatalf("unexpected error heartbeat: %+v", hb)
	}
	if hb.LastSuccessAt != firstSuccess {
		t.Fatalf("error heartbeat should preserve last_success_at: got %q want %q", hb.LastSuccessAt, firstSuccess)
	}

	// Still a single upserted row.
	var n int
	if err := repo.db.QueryRow("SELECT COUNT(*) FROM metadata WHERE key = 'scraper_heartbeat'").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 heartbeat row, got %d", n)
	}
}

func TestOpenMigratesFreshDB(t *testing.T) {
	repo := newTestRepo(t)

	// goose recorded the baseline (1), user_id (2), company_registry (3),
	// api_usage tokens (4), global_jobs (5), the global_jobs backfill (6),
	// dashboard latency indexes (7), jobs.archive_reason (8), scoring cascade
	// provenance (9), and the cascade drop (10).
	var version int
	if err := repo.db.QueryRow(
		"SELECT MAX(version_id) FROM goose_db_version",
	).Scan(&version); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	if version != 10 {
		t.Fatalf("goose version = %d, want 10", version)
	}

	// A v27 column must exist, proving the full baseline ran (not a stub).
	var dummy sql.NullInt64
	if err := repo.db.QueryRow("SELECT score_attempts FROM jobs LIMIT 1").Scan(&dummy); err != nil && err != sql.ErrNoRows {
		t.Fatalf("score_attempts column missing: %v", err)
	}

	// The user_id migration (00002) ran: the tenant column exists.
	var uid sql.NullString
	if err := repo.db.QueryRow("SELECT user_id FROM jobs LIMIT 1").Scan(&uid); err != nil && err != sql.ErrNoRows {
		t.Fatalf("user_id column missing: %v", err)
	}

	// A late baseline table must exist too.
	exists, err := tableExists(repo.db, "ats_resolution_cache")
	if err != nil {
		t.Fatalf("tableExists: %v", err)
	}
	if !exists {
		t.Fatal("ats_resolution_cache table missing after migrate")
	}
	exists, err = tableExists(repo.db, "global_jobs")
	if err != nil {
		t.Fatalf("tableExists global_jobs: %v", err)
	}
	if !exists {
		t.Fatal("global_jobs table missing after migrate")
	}
	for _, name := range []string{
		"idx_jobs_dash_not_applied_score_page",
		"idx_events_stage_user_job_created_id",
		"idx_events_user_to_created_job",
	} {
		if !sqliteIndexExists(t, repo.db, name) {
			t.Fatalf("dashboard latency index %s missing after migrate", name)
		}
	}
}

// TestStampLegacyBaseline verifies a live Node DB (already at schema_version 27,
// no goose table) is stamped as baselined without the baseline DDL being replayed.
func TestStampLegacyBaseline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Build a realistic "legacy" DB: the full set of baseline tables a v27 Node DB
	// has (but no goose table and no baseline indexes), plus the schema_version
	// marker and a seeded job row. The user_id migration (00002) must ALTER these
	// real tables, while the baseline itself must be stamped, not replayed.
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	if _, err := seed.Exec(`
		CREATE TABLE jobs (id TEXT PRIMARY KEY, title TEXT, company TEXT, url TEXT, platform TEXT, location TEXT, posted_at TEXT, scraped_at TEXT, description TEXT, score INTEGER, reasoning TEXT, status TEXT, stage TEXT, created_at TEXT, updated_at TEXT, score_attempts INTEGER);
		CREATE TABLE metadata (key TEXT PRIMARY KEY, value TEXT);
		CREATE TABLE api_usage (date TEXT, model TEXT, call_count INTEGER, PRIMARY KEY(date, model));
		CREATE TABLE company_notes (company TEXT PRIMARY KEY, tags TEXT, notes TEXT);
		CREATE TABLE events (id INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT, event_type TEXT, to_value TEXT, created_at TEXT);
		CREATE TABLE rejection_email_log (id INTEGER PRIMARY KEY AUTOINCREMENT, mailbox TEXT, uid_validity TEXT, uid INTEGER, match_status TEXT);
		CREATE TABLE job_aliases (alternate_job_id TEXT PRIMARY KEY, canonical_job_id TEXT, status TEXT);
		CREATE TABLE status_snapshots (id INTEGER PRIMARY KEY AUTOINCREMENT, recorded_at TEXT);
		CREATE TABLE ats_resolution_cache (job_id TEXT PRIMARY KEY, outcome TEXT);
		INSERT INTO metadata (key, value) VALUES ('schema_version', '27');
		INSERT INTO jobs (id, title, company) VALUES ('legacy-1', 'Engineer', 'Acme');
	`); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	seed.Close()

	repo, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy: %v", err)
	}
	defer repo.Close()

	// Baseline stamped (1), then user_id (2), company_registry (3),
	// api_usage tokens (4), global_jobs (5), global_jobs backfill (6),
	// dashboard latency indexes (7), jobs.archive_reason (8), scoring cascade
	// provenance (9), and the cascade drop (10) applied.
	var version int
	if err := repo.db.QueryRow("SELECT MAX(version_id) FROM goose_db_version").Scan(&version); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	if version != 10 {
		t.Fatalf("goose version = %d, want 10", version)
	}

	// Baseline was NOT replayed: a baseline index the seed lacks (and that 00002
	// does not create) is still absent.
	var idx string
	err = repo.db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='index' AND name='idx_jobs_status'",
	).Scan(&idx)
	if err != sql.ErrNoRows {
		t.Fatalf("baseline was replayed against the legacy DB (idx_jobs_status present); it should have been stamped only: %v", err)
	}

	// The user_id migration ran against the legacy tables and backfilled 'local'.
	var uid string
	if err := repo.db.QueryRow("SELECT user_id FROM jobs WHERE id='legacy-1'").Scan(&uid); err != nil {
		t.Fatalf("user_id not added to legacy jobs: %v", err)
	}
	if uid != LocalUser {
		t.Fatalf("legacy job user_id = %q, want %q", uid, LocalUser)
	}
}

func sqliteIndexExists(t *testing.T, conn *sql.DB, name string) bool {
	t.Helper()
	var got string
	err := conn.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='index' AND name=?",
		name,
	).Scan(&got)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatalf("lookup index %s: %v", name, err)
	}
	return true
}
