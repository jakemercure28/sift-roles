// Package db is the Go owner of the shared jobs.db. It manages the schema via
// embedded goose migrations (see migrate.go) and owns the scraped-lead insert
// path and event telemetry. Scoring, canonicalization, and stats are being
// migrated from the Node side (lib/db.js, lib/db/schema.js) into Go.
package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx" for Postgres
	_ "modernc.org/sqlite"

	"job-search-automation/internal/model"
)

// LocalUser is the tenant id used for the single-tenant SQLite self-host
// backend. Every row a self-host install writes is owned by this user, and all
// queries filter by it, so the multi-tenant query shape is exercised even with
// one user. Hosted Postgres replaces it per request via ForUser.
const LocalUser = "local"

// HostKeyUser is a reserved pseudo-tenant that tallies Gemini calls made on the
// shared host key across all tenants, so the host key has one global daily ceiling
// independent of any single tenant's per-tenant limit. It never owns job rows.
const HostKeyUser = "__host__"

// Repository is a handle to the shared database (SQLite self-host or hosted
// Postgres). dl rewrites SQLite-dialect SQL for the active backend; userID is
// the tenant every query is scoped to.
type Repository struct {
	db     *sql.DB
	dl     dialect
	userID string
	// rls activates Postgres row-level security as defense-in-depth (RLS_ENFORCE).
	// When true, every query runs inside a transaction that first sets the
	// app.user_id GUC the 00002_rls.sql policies key on, so an accidental unscoped
	// query fails closed. Only set on a Postgres repo connected as a restricted
	// (non-BYPASSRLS) role; false leaves the SQLite/self-host and current Postgres
	// paths byte-for-byte unchanged.
	rls bool
}

// ForUser returns a shallow copy of the repository scoped to a different tenant.
// Self-host stays on LocalUser; the hosted backend will call this per request
// once the auth middleware (Phase 3) resolves the caller's user id.
func (r *Repository) ForUser(userID string) *Repository {
	clone := *r
	clone.userID = userID
	return &clone
}

// UserID is the tenant this repository is scoped to.
func (r *Repository) UserID() string { return r.userID }

// DBType is the storage backend this repository is bound to.
func (r *Repository) DBType() DBType { return r.dl.kind }

// ActiveTenant resolves the user_id whose on-disk profile the background
// pipeline (discovery, scoring, market research, slug-health, scraping) should
// operate on. The background workers run outside any request, so they cannot
// inherit a tenant from the auth middleware the way the dashboard does; they ask
// here instead.
//
// Self-host SQLite is single-tenant, so it always returns LocalUser without a
// query, keeping that path byte-for-byte unchanged. Hosted Postgres returns the
// non-local tenant that owns the most job rows (the only human user on a
// single-user deployment); querying by row count means it degrades gracefully to
// the dominant tenant rather than failing if a stray second tenant ever appears.
// The synthetic LocalUser partition is ignored on Postgres because it is only a
// self-host fallback/orphan bucket, never a hosted user. Returns "" (no error)
// when no tenant owns any jobs yet, which callers treat as "fall back to the
// root data dir".
func (r *Repository) ActiveTenant() (string, error) {
	if r.DBType() != Postgres {
		return LocalUser, nil
	}
	rows, err := r.query(`SELECT user_id FROM jobs WHERE user_id <> '' AND user_id <> ? GROUP BY user_id ORDER BY COUNT(*) DESC`, LocalUser)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var top string
	if rows.Next() {
		if err := rows.Scan(&top); err != nil {
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return top, nil
}

// Tenants returns every tenant the background pipeline should service, so the
// crons can fan out per user instead of only touching ActiveTenant (the dominant
// one). Self-host SQLite returns exactly [LocalUser] without a query, keeping that
// path byte-for-byte unchanged. Hosted Postgres returns the distinct non-local,
// non-empty user_ids that own at least one job row.
//
// A tenant that is provisioned but has not been scraped yet owns no rows and so is
// not returned here; that first scrape is driven interactively from the setup
// wizard, after which the tenant appears and the crons pick it up.
func (r *Repository) Tenants() ([]string, error) {
	if r.DBType() != Postgres {
		return []string{LocalUser}, nil
	}
	rows, err := r.query(`SELECT DISTINCT user_id FROM jobs WHERE user_id <> '' AND user_id <> ?`, LocalUser)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tenants []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		tenants = append(tenants, uid)
	}
	return tenants, rows.Err()
}

// BackgroundTenants is the tenant set the background crons should fan out over:
// the union of tenants that already own job rows (Tenants) and tenants whose
// on-disk profile is onboarded but jobless (OnboardedTenantDirs under base, the
// configured root data dir / cfg.DataDir).
//
// Keying the fan-out on job rows alone created a bootstrap deadlock: a freshly
// onboarded tenant owns no rows, so every cron (scrape, scoring, discovery,
// maintenance) skipped it; but it could only get rows once a scrape ran, and the
// only path that scraped a jobless tenant was a manual "Scrape now" click. Miss
// that one click and the tenant sat at zero forever, silently. Unioning the
// onboarded-on-disk set makes the crons self-healing: discovery (in the
// maintenance fan-out) populates the tenant's companies, the next scrape produces
// rows, and the tenant joins the jobs-derived set on its own.
//
// Self-host SQLite returns exactly Tenants() (no per-tenant storage root), keeping
// that path byte-for-byte unchanged.
func (r *Repository) BackgroundTenants(base string) ([]string, error) {
	tenants, err := r.Tenants()
	if err != nil {
		return nil, err
	}
	if r.DBType() != Postgres {
		return tenants, nil
	}
	seen := make(map[string]bool, len(tenants))
	for _, uid := range tenants {
		seen[uid] = true
	}
	for _, uid := range OnboardedTenantDirs(base, r.DBType()) {
		if !seen[uid] {
			seen[uid] = true
			tenants = append(tenants, uid)
		}
	}
	return tenants, nil
}

// ProfileDir resolves the on-disk profile/storage directory the background
// pipeline should read from, given the configured root data dir (cfg.DataDir).
// Self-host SQLite gets base unchanged; hosted Postgres gets the active tenant's
// isolated dir (base/storage/users/{uid}) via the same TenantDataDir mapping the
// dashboard and migrator use. It is the single source of truth shared by the Go
// cron tasks (cmd/server) and the scrape scheduler. On any resolution failure it
// falls back to base: a real DB outage surfaces loudly in the task's own queries
// rather than here.
func (r *Repository) ProfileDir(base string) string {
	uid, err := r.ActiveTenant()
	if err != nil || uid == "" {
		return base
	}
	return TenantDataDir(base, r.DBType(), uid)
}

// Rows is the read-cursor surface the data layer uses. *sql.Rows satisfies it
// directly; under RLS, query returns a wrapper whose Close also commits the
// per-query transaction (releasing the pooled connection).
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// Row is the single-row surface the data layer uses. *sql.Row satisfies it;
// under RLS, queryRow returns a wrapper that commits the transaction after Scan.
type Row interface {
	Scan(dest ...any) error
}

// setRLSUserSQL stamps the per-transaction app.user_id GUC the 00002_rls.sql
// policies match on. is_local=true scopes it to the current transaction, which is
// the only safe scope over the Supabase transaction pooler (a session-level SET
// would leak to the next tenant on the same pooled backend). It is raw Postgres,
// so it bypasses the dialect rewriter.
const setRLSUserSQL = "SELECT set_config('app.user_id', $1, true)"

// exec, query, and queryRow run a statement through the active dialect's
// rewriter before executing, so call sites keep writing SQLite-dialect SQL.
// When r.rls is set (Postgres defense-in-depth), each runs inside its own
// transaction that first sets app.user_id; otherwise they hit the pool directly,
// leaving the self-host and current Postgres paths unchanged.
func (r *Repository) exec(query string, args ...any) (sql.Result, error) {
	if !r.rls {
		return r.db.Exec(r.dl.rewrite(query), args...)
	}
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(setRLSUserSQL, r.userID); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	res, err := tx.Exec(r.dl.rewrite(query), args...)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

func (r *Repository) query(query string, args ...any) (Rows, error) {
	if !r.rls {
		return r.db.Query(r.dl.rewrite(query), args...)
	}
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(setRLSUserSQL, r.userID); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	rows, err := tx.Query(r.dl.rewrite(query), args...)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return &rlsRows{Rows: rows, tx: tx}, nil
}

func (r *Repository) queryRow(query string, args ...any) Row {
	if !r.rls {
		return r.db.QueryRow(r.dl.rewrite(query), args...)
	}
	tx, err := r.db.Begin()
	if err != nil {
		return errRow{err: err}
	}
	if _, err := tx.Exec(setRLSUserSQL, r.userID); err != nil {
		_ = tx.Rollback()
		return errRow{err: err}
	}
	return &rlsRow{row: tx.QueryRow(r.dl.rewrite(query), args...), tx: tx}
}

// rlsRows ties the cursor of an RLS-scoped transaction to that transaction so the
// connection is released (commit) when the caller closes the rows. Read-only, so
// commit vs rollback is immaterial; commit is the cheaper close.
type rlsRows struct {
	*sql.Rows
	tx *sql.Tx
}

func (r *rlsRows) Close() error {
	err := r.Rows.Close()
	if cErr := r.tx.Commit(); err == nil {
		err = cErr
	}
	return err
}

// rlsRow commits the RLS-scoped transaction once Scan has read the single row,
// preserving sql.ErrNoRows for callers that branch on it.
type rlsRow struct {
	row *sql.Row
	tx  *sql.Tx
}

func (r *rlsRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	if cErr := r.tx.Commit(); err == nil {
		err = cErr
	}
	return err
}

// errRow defers a setup error (e.g. a failed BEGIN) to Scan, so queryRow keeps
// its error-free signature the way *sql.Row does.
type errRow struct{ err error }

func (e errRow) Scan(dest ...any) error { return e.err }

// execer wraps a transaction (or any sqlExec) so queries run through the dialect
// rewriter inside transactions too. It satisfies sqlExec (see canonicalize.go).
type execer struct {
	raw sqlExec
	dl  dialect
}

func (e execer) Exec(query string, args ...any) (sql.Result, error) {
	return e.raw.Exec(e.dl.rewrite(query), args...)
}

func (e execer) QueryRow(query string, args ...any) *sql.Row {
	return e.raw.QueryRow(e.dl.rewrite(query), args...)
}

// RawDB exposes the underlying SQL handle for migration packages that need a
// broad transactional surface while behavior is being ported from Node.
func (r *Repository) RawDB() *sql.DB { return r.db }

// Open connects to jobs.db at dbPath, applies the pragmas (WAL, busy_timeout,
// foreign_keys, synchronous), and brings the schema up to date via the embedded
// goose migrations. A fresh database gets the full schema; a live Node database
// is stamped as already-baselined without replaying DDL (see Migrate).
//
// busy_timeout is now 5000 because Go owns the scheduled DB writes; the larger
// 15000 window was only needed while the Node worker shared write ownership.
func Open(dbPath string) (*Repository, error) {
	// busy_timeout/foreign_keys/synchronous are per-connection; set them via the
	// DSN so every pooled connection inherits them (matches lib/db.js).
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)&_pragma=synchronous(normal)",
		dbPath,
	)
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}

	if err := sqldb.Ping(); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}

	// journal_mode is persisted in the file; switch to WAL only if not already
	// (matches lib/db.js, which avoids needless mode-switch locking).
	var mode string
	if err := sqldb.QueryRow("PRAGMA journal_mode").Scan(&mode); err == nil && mode != "wal" {
		if _, err := sqldb.Exec("PRAGMA journal_mode=WAL"); err != nil {
			sqldb.Close()
			return nil, fmt.Errorf("set WAL: %w", err)
		}
	}

	dl := dialect{kind: SQLite}
	if err := Migrate(sqldb, dl); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("migrate %s: %w", dbPath, err)
	}

	return &Repository{db: sqldb, dl: dl, userID: LocalUser}, nil
}

// PoolConfig tunes the database/sql connection pool for the hosted Postgres
// backend. It is sized for a Supavisor/PgBouncer transaction pooler (Supabase's
// port 6543), which multiplexes many client connections onto far fewer server
// connections, so the app keeps its own pool deliberately small.
type PoolConfig struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// DefaultPoolConfig returns conservative pool settings safe behind a transaction
// pooler. Callers (cmd/server) override these from DB_* env vars.
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MaxOpenConns:    10,
		MaxIdleConns:    5,
		ConnMaxLifetime: 5 * time.Minute,
		ConnMaxIdleTime: 5 * time.Minute,
	}
}

// OpenPostgres connects to the hosted Postgres backend at dsn (the multi-tenant
// SaaS path) and brings the schema up to date via the Postgres migration track.
// The query layer rewrites SQLite-dialect SQL to Postgres on the fly (see
// dialect.go), so the same Repository methods serve both backends.
//
// dsn typically points at a transaction pooler (Supabase Supavisor on :6543),
// which does not preserve session state across pooled checkouts. pgx's default
// extended-query protocol leans on server-side prepared statements that break in
// that mode, so we force the simple query protocol. The pool is also bounded
// (pool) because the pooler, not us, owns the real server-connection budget.
func OpenPostgres(dsn string, pool PoolConfig, tz string, rls bool) (*Repository, error) {
	connConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	// Transaction-pooler safety: no named/server-side prepared statements.
	connConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	sqldb, err := sql.Open("pgx", stdlib.RegisterConnConfig(connConfig))
	if err != nil {
		return nil, err
	}

	sqldb.SetMaxOpenConns(pool.MaxOpenConns)
	sqldb.SetMaxIdleConns(pool.MaxIdleConns)
	sqldb.SetConnMaxLifetime(pool.ConnMaxLifetime)
	sqldb.SetConnMaxIdleTime(pool.ConnMaxIdleTime)

	if err := sqldb.Ping(); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("open postgres: %w", err)
	}

	dl := dialect{kind: Postgres, tz: tz}
	// Migrations need owner privileges. The restricted (non-BYPASSRLS) role the RLS
	// serving connection uses can't run DDL, so when rls is set the caller has
	// already migrated through the service-role connection (see dashboardRepo in
	// cmd/server); skip migration here.
	if !rls {
		if err := Migrate(sqldb, dl); err != nil {
			sqldb.Close()
			return nil, fmt.Errorf("migrate postgres: %w", err)
		}
	}

	return &Repository{db: sqldb, dl: dl, userID: LocalUser, rls: rls}, nil
}

// Rewrite exposes the active dialect's SQL rewriter for data-layer packages that
// hold the raw *sql.DB via RawDB (e.g. rejectionsync, contextupdate).
func (r *Repository) Rewrite(query string) string { return r.dl.rewrite(query) }

// Close closes the underlying database.
func (r *Repository) Close() error { return r.db.Close() }

const insertLeadSQL = `
INSERT OR IGNORE INTO jobs
  (user_id, id, global_job_id, title, company, url, platform, location, posted_at, scraped_at, description, status)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')`

const patchLocationSQL = `
UPDATE jobs SET location = ? WHERE id = ? AND user_id = ? AND (location = '' OR location IS NULL)`

const patchDescriptionSQL = `
UPDATE jobs
SET description = ?, updated_at = datetime('now')
WHERE id = ? AND user_id = ? AND (description = '' OR description IS NULL)`

// InsertScrapedLead inserts a scraped lead as a new pending/unscored row,
// mirroring the new-row path of importJob in lib/db.js. It is idempotent:
// re-inserting an existing id is a no-op (INSERT OR IGNORE). Returns whether a
// new row was inserted.
func (r *Repository) InsertScrapedLead(lead model.Lead) (bool, error) {
	return r.insertScrapedLead(lead, true)
}

func (r *Repository) insertScrapedLead(lead model.Lead, harvestGlobal bool) (bool, error) {
	if harvestGlobal {
		if err := r.HarvestGlobalLead(lead); err != nil {
			return false, err
		}
	}
	rowID := r.JobRowID(lead.ID)
	res, err := r.exec(insertLeadSQL,
		r.userID,
		rowID,
		lead.ID,
		lead.Title,
		lead.Company,
		lead.DirectApplyURL,
		lead.ATSPlatformName,
		lead.Location,
		normalizePostedAt(lead.PostedAt, time.Now().UTC()),
		lead.ScrapedTimestamp,
		lead.Description,
	)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}

	// Backfill location on a pre-existing row that was stored empty (matches
	// the location patch in importJob).
	if lead.Location != "" {
		if _, err := r.exec(patchLocationSQL, lead.Location, rowID, r.userID); err != nil {
			return affected > 0, err
		}
	}
	if lead.Description != "" {
		if _, err := r.exec(patchDescriptionSQL, lead.Description, rowID, r.userID); err != nil {
			return affected > 0, err
		}
	}
	return affected > 0, nil
}

// LogEvent writes an audit-trail row into events, mirroring logEvent in
// lib/db.js. Empty from/to values are stored as NULL.
func (r *Repository) LogEvent(jobID, eventType, from, to string) error {
	_, err := r.exec(
		"INSERT INTO events (user_id, job_id, event_type, from_value, to_value) VALUES (?, ?, ?, ?, ?)",
		r.userID, jobID, eventType, nullable(from), nullable(to),
	)
	return err
}

// ExistingJobKeys returns the set of LOWER(TRIM(title))|||LOWER(TRIM(company))
// keys already in jobs, mirroring getExistingJobKeys in lib/db.js. Useful for
// batch pre-filtering before insert.
func (r *Repository) ExistingJobKeys() (map[string]struct{}, error) {
	rows, err := r.query(
		"SELECT LOWER(TRIM(title)) || '|||' || LOWER(TRIM(company)) FROM jobs WHERE user_id = ?",
		r.userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := make(map[string]struct{})
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys[key] = struct{}{}
	}
	return keys, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Heartbeat is the authoritative scrape-liveness signal the dashboard reads. It
// is written after every cycle (success and failure) to metadata key
// 'scraper_heartbeat', so the dashboard can distinguish "scraped, nothing new"
// from "never ran / failed".
type Heartbeat struct {
	LastAttemptAt string `json:"last_attempt_at"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	Status        string `json:"status"` // "ok" | "error"
	Scraped       int    `json:"scraped"`
	Inserted      int    `json:"inserted"`
	Error         string `json:"error,omitempty"`
}

const heartbeatKey = "scraper_heartbeat"

func (r *Repository) readHeartbeat() Heartbeat {
	var raw string
	err := r.queryRow("SELECT value FROM metadata WHERE key = ? AND user_id = ?", heartbeatKey, r.userID).Scan(&raw)
	if err != nil {
		return Heartbeat{}
	}
	var hb Heartbeat
	if err := json.Unmarshal([]byte(raw), &hb); err != nil {
		return Heartbeat{}
	}
	return hb
}

// WriteHeartbeat upserts the scrape heartbeat. On success (status "ok") it
// stamps last_success_at; on error it preserves the prior last_success_at so the
// dashboard can show both "last attempt failed" and "last good scrape was N ago".
func (r *Repository) WriteHeartbeat(status string, scraped, inserted int, scrapeErr string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	hb := Heartbeat{
		LastAttemptAt: now,
		LastSuccessAt: r.readHeartbeat().LastSuccessAt,
		Status:        status,
		Scraped:       scraped,
		Inserted:      inserted,
		Error:         scrapeErr,
	}
	if status == "ok" {
		hb.LastSuccessAt = now
	}
	payload, err := json.Marshal(hb)
	if err != nil {
		return err
	}
	_, err = r.exec(
		`INSERT INTO metadata (user_id, key, value, updated_at) VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		r.userID, heartbeatKey, string(payload),
	)
	return err
}
