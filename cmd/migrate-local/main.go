// Command migrate-local is the one-shot lift-and-shift that moves a self-host
// SQLite history into the hosted Postgres backend under a single Supabase tenant
// id. It reuses the production engine instead of re-implementing SQL: the target
// is opened via db.OpenPostgres (which also applies the Postgres migration
// track), rows are translated with the same dialect rewriter the app uses
// (Repository.Rewrite), and per-tenant asset paths come from db.TenantDataDir.
//
// It copies every row of the tenant-owned history tables, stamps the target
// user_id on each, and is safe to re-run: rows already present are skipped via ON
// CONFLICT DO NOTHING. Runtime global caches (company_registry, global_jobs) are
// intentionally rebuildable and are not required for history migration. Identity
// (BIGINT GENERATED ALWAYS) ids are preserved with OVERRIDING SYSTEM VALUE so the
// primary key drives that idempotency, and the identity sequences are advanced
// past the migrated max afterward so future app inserts don't collide. Rows whose
// foreign key to jobs(id) points at a job not present in the source are skipped as
// orphans to keep referential integrity intact.
//
// Because the live database runs inside the Docker volume, copy it out first:
//
//	docker cp job-search-automation-go-backend-1:/app/db/jobs.db /tmp/jobs.db
//	DATABASE_URL='postgres://...@<proj>.supabase.co:6543/postgres?sslmode=require' \
//	  go run ./cmd/migrate-local --sqlite /tmp/jobs.db --user-id <supabase-uid> --dry-run
//	# review the per-table counts, then re-run without --dry-run.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"job-search-automation/internal/db"

	_ "modernc.org/sqlite"
)

// migrationOrder lists the tenant-owned history tables jobs-first so foreign keys
// to jobs(id) resolve against already-inserted parents within the single
// transaction.
var migrationOrder = []string{
	"jobs",
	"metadata",
	"api_usage",
	"company_notes",
	"events",
	"rejection_email_log",
	"job_aliases",
	"status_snapshots",
	"ats_resolution_cache",
}

// identityTables use BIGINT GENERATED ALWAYS AS IDENTITY for their id. Inserting
// an explicit id needs OVERRIDING SYSTEM VALUE, and their sequences are reset
// after the copy.
var identityTables = map[string]bool{
	"events":              true,
	"rejection_email_log": true,
	"status_snapshots":    true,
}

// jobFKCols maps each table to the columns that reference jobs(id). A row whose
// non-null value here is absent from the migrated job set is an orphan and is
// skipped so the Postgres foreign keys hold.
var jobFKCols = map[string][]string{
	"events":               {"job_id"},
	"rejection_email_log":  {"matched_job_id"},
	"job_aliases":          {"alternate_job_id", "canonical_job_id"},
	"ats_resolution_cache": {"job_id"},
}

// profileAssets are the root data/ files and directories lifted into the
// tenant's isolated storage dir. Missing entries are skipped.
var profileAssets = []string{
	"resume.md",
	"context.md",
	"career-detail.md",
	"companies.json",
	"location.json",
	"suggested-companies.json",
	"experience",
	"tailored-resumes",
	// The onboarding marker gates automated scraping (lib/onboarding). It must
	// travel with the profile, or the relocated tenant reads as un-onboarded and
	// the worker skips every scrape.
	".onboarded",
}

const batchRows = 100

type options struct {
	sqlitePath  string
	userID      string
	databaseURL string
	dataDir     string
	dryRun      bool
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate-local: "+err.Error())
		os.Exit(1)
	}
}

func run(argv []string) error {
	opts, err := parseArgs(argv)
	if err != nil {
		return err
	}

	// Open read-write (not mode=ro): the source is a throwaway copy, and the
	// pure-Go driver under-reads a WAL-mode database opened strictly read-only.
	// foreign_keys stays off so legacy orphan rows are readable.
	src, err := sql.Open("sqlite", "file:"+opts.sqlitePath+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(off)")
	if err != nil {
		return fmt.Errorf("open source sqlite: %w", err)
	}
	defer src.Close()
	if err := src.Ping(); err != nil {
		return fmt.Errorf("open source sqlite %q: %w", opts.sqlitePath, err)
	}

	if opts.dryRun {
		return dryRun(src, opts)
	}

	// tz is irrelevant for a one-shot row copy (it only affects 'localtime' date
	// rewrites at query time), so leave it at the backend default.
	dest, err := db.OpenPostgres(opts.databaseURL, db.DefaultPoolConfig(), "", false)
	if err != nil {
		return fmt.Errorf("open target postgres: %w", err)
	}
	defer dest.Close()

	jobIDs, err := loadJobIDs(src)
	if err != nil {
		return fmt.Errorf("load source job ids: %w", err)
	}
	fmt.Printf("source jobs: %d\n", len(jobIDs))

	ctx := context.Background()
	tx, err := dest.RawDB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	for _, table := range migrationOrder {
		res, err := migrateTable(ctx, tx, src, dest.Rewrite, table, opts.userID, jobIDs)
		if err != nil {
			return fmt.Errorf("migrate %s: %w", table, err)
		}
		fmt.Printf("%-22s inserted=%-7d skipped(conflict)=%-7d skipped(orphan)=%d\n",
			table, res.inserted, res.conflict, res.orphan)
	}

	if err := resetIdentitySequences(ctx, tx); err != nil {
		return fmt.Errorf("reset identity sequences: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	if err := moveAssets(opts.dataDir, opts.userID, false); err != nil {
		return fmt.Errorf("move profile assets: %w", err)
	}

	fmt.Printf("done: history migrated under user_id=%q\n", opts.userID)
	return nil
}

type tableResult struct {
	inserted int64
	conflict int64
	orphan   int64
}

// migrateTable copies one table from SQLite into the Postgres transaction.
// It reads columns dynamically (SELECT *) so no per-table struct is needed,
// overrides user_id with the target tenant, drops orphaned foreign keys, and
// batches INSERT ... ON CONFLICT DO NOTHING through the dialect rewriter.
func migrateTable(ctx context.Context, tx *sql.Tx, src *sql.DB, rewrite func(string) string, table, userID string, jobIDs map[string]bool) (tableResult, error) {
	var res tableResult

	rows, err := src.Query("SELECT * FROM " + table) //nolint:gosec // table is a fixed constant from migrationOrder
	if err != nil {
		return res, err
	}
	defer rows.Close()

	srcCols, err := rows.Columns()
	if err != nil {
		return res, err
	}
	dstCols, err := targetColumns(ctx, tx, table)
	if err != nil {
		return res, err
	}
	// Copy only columns the target schema actually has. The live SQLite carries
	// legacy Node-era columns (transcript_path, auto_apply_*, rejection_analysis)
	// the Postgres schema intentionally drops, so project source rows down to the
	// intersection. useIdx maps each kept column back to its source position.
	var useCols []string
	var useIdx []int
	for i, c := range srcCols {
		if dstCols[c] {
			useCols = append(useCols, c)
			useIdx = append(useIdx, i)
		}
	}
	userIdx := indexOf(srcCols, "user_id")
	fkIdx := make([]int, 0, len(jobFKCols[table]))
	for _, c := range jobFKCols[table] {
		if i := indexOf(srcCols, c); i >= 0 {
			fkIdx = append(fkIdx, i)
		}
	}
	overriding := identityTables[table]

	batch := make([][]any, 0, batchRows)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		affected, err := insertBatch(ctx, tx, rewrite, table, useCols, overriding, batch)
		if err != nil {
			return err
		}
		res.inserted += affected
		res.conflict += int64(len(batch)) - affected
		batch = batch[:0]
		return nil
	}

	for rows.Next() {
		vals := make([]any, len(srcCols))
		ptrs := make([]any, len(srcCols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return res, err
		}
		for i, v := range vals {
			// pgx simple protocol encodes []byte as bytea; SQLite hands TEXT back
			// as []byte for some columns, so coerce to string for the text schema.
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		if userIdx >= 0 {
			vals[userIdx] = userID
		}
		if isOrphan(vals, fkIdx, jobIDs) {
			res.orphan++
			continue
		}
		row := make([]any, len(useIdx))
		for j, si := range useIdx {
			row[j] = vals[si]
		}
		batch = append(batch, row)
		if len(batch) >= batchRows {
			if err := flush(); err != nil {
				return res, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	if err := flush(); err != nil {
		return res, err
	}
	return res, nil
}

// insertBatch builds a multi-row INSERT for cols, runs it through the dialect
// rewriter (which rebinds ? to $N), and returns the number of rows actually
// inserted (conflicts are swallowed by ON CONFLICT DO NOTHING).
func insertBatch(ctx context.Context, tx *sql.Tx, rewrite func(string) string, table string, cols []string, overriding bool, batch [][]any) (int64, error) {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(table)
	b.WriteString(" (")
	b.WriteString(strings.Join(cols, ", "))
	b.WriteString(") ")
	if overriding {
		b.WriteString("OVERRIDING SYSTEM VALUE ")
	}
	b.WriteString("VALUES ")

	args := make([]any, 0, len(batch)*len(cols))
	for i, row := range batch {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for j := range cols {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteByte('?')
			args = append(args, row[j])
		}
		b.WriteByte(')')
	}
	b.WriteString(" ON CONFLICT DO NOTHING")

	resExec, err := tx.ExecContext(ctx, rewrite(b.String()), args...)
	if err != nil {
		return 0, err
	}
	affected, err := resExec.RowsAffected()
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// targetColumns returns the set of columns the target Postgres table actually
// has, so the copy can drop legacy source-only columns.
func targetColumns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT * FROM "+table+" LIMIT 0") //nolint:gosec // table is a fixed constant from migrationOrder
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(cols))
	for _, c := range cols {
		set[c] = true
	}
	return set, nil
}

// resetIdentitySequences advances each identity table's sequence past the
// migrated max id so the next app-generated row doesn't collide with a
// preserved id.
func resetIdentitySequences(ctx context.Context, tx *sql.Tx) error {
	for table := range identityTables {
		q := fmt.Sprintf(
			`SELECT setval(pg_get_serial_sequence('%[1]s','id'), GREATEST(COALESCE((SELECT MAX(id) FROM %[1]s), 0), 1), (SELECT COUNT(*) > 0 FROM %[1]s))`,
			table,
		)
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
	}
	return nil
}

// loadJobIDs reads the full set of job ids from the source so dependent tables
// can drop orphaned foreign keys.
func loadJobIDs(src *sql.DB) (map[string]bool, error) {
	rows, err := src.Query("SELECT id FROM jobs")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// isOrphan reports whether any non-null foreign-key column points at a job id
// absent from the migrated set.
func isOrphan(vals []any, fkIdx []int, jobIDs map[string]bool) bool {
	for _, i := range fkIdx {
		v := vals[i]
		if v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		if !jobIDs[s] {
			return true
		}
	}
	return false
}

// dryRun reports per-table source row counts and the planned asset moves without
// touching Postgres.
func dryRun(src *sql.DB, opts options) error {
	fmt.Println("DRY RUN (no writes)")
	for _, table := range migrationOrder {
		var n int64
		if err := src.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil { //nolint:gosec // fixed table
			return fmt.Errorf("count %s: %w", table, err)
		}
		fmt.Printf("%-22s rows=%d\n", table, n)
	}
	return moveAssets(opts.dataDir, opts.userID, true)
}

// moveAssets relocates the root profile files/dirs into the tenant's isolated
// storage directory (data/storage/users/{user_id}/). It uses os.Rename (the
// destination is a subdirectory of the same filesystem), skips entries already
// moved, and ignores missing ones.
func moveAssets(dataDir, userID string, dryRun bool) error {
	destDir := db.TenantDataDir(dataDir, db.Postgres, userID)
	if destDir == dataDir {
		return fmt.Errorf("refusing to move assets: tenant dir resolves to the base dir (user-id %q)", userID)
	}
	if !dryRun {
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return err
		}
	}
	for _, name := range profileAssets {
		srcPath := filepath.Join(dataDir, name)
		if _, err := os.Stat(srcPath); err != nil {
			continue // not present, nothing to move
		}
		dstPath := filepath.Join(destDir, name)
		if _, err := os.Stat(dstPath); err == nil {
			fmt.Printf("asset %-24s already present at %s (skip)\n", name, dstPath)
			continue
		}
		if dryRun {
			fmt.Printf("asset %-24s would move -> %s\n", name, dstPath)
			continue
		}
		if err := os.Rename(srcPath, dstPath); err != nil {
			return fmt.Errorf("move %s: %w", name, err)
		}
		fmt.Printf("asset %-24s moved -> %s\n", name, dstPath)
	}
	return nil
}

func indexOf(ss []string, target string) int {
	for i, s := range ss {
		if s == target {
			return i
		}
	}
	return -1
}

func parseArgs(argv []string) (options, error) {
	opts := options{
		sqlitePath:  "data/jobs.db",
		userID:      os.Getenv("TARGET_USER_ID"),
		databaseURL: os.Getenv("DATABASE_URL"),
		dataDir:     "data",
	}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch a {
		case "-h", "--help":
			fmt.Print(usage)
			os.Exit(0)
		case "--dry-run":
			opts.dryRun = true
			continue
		}
		if !strings.HasPrefix(a, "--") {
			return opts, fmt.Errorf("unexpected argument %q\n\n%s", a, usage)
		}
		key := strings.TrimPrefix(a, "--")
		val := ""
		if eq := strings.IndexByte(key, '='); eq >= 0 {
			key, val = key[:eq], key[eq+1:]
		} else if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
			val = argv[i+1]
			i++
		}
		switch key {
		case "sqlite":
			opts.sqlitePath = val
		case "user-id":
			opts.userID = val
		case "data-dir":
			opts.dataDir = val
		case "database-url":
			opts.databaseURL = val
		default:
			return opts, fmt.Errorf("unknown flag --%s\n\n%s", key, usage)
		}
	}

	if opts.userID == "" {
		return opts, fmt.Errorf("--user-id (or TARGET_USER_ID) is required\n\n%s", usage)
	}
	if opts.userID == db.LocalUser {
		return opts, fmt.Errorf("--user-id %q is the self-host sentinel; pass a real Supabase user id", db.LocalUser)
	}
	if !opts.dryRun && opts.databaseURL == "" {
		return opts, fmt.Errorf("DATABASE_URL (or --database-url) is required for a live run\n\n%s", usage)
	}
	return opts, nil
}

const usage = `migrate-local: lift a self-host SQLite history into hosted Postgres under one tenant.

Usage:
  migrate-local --user-id <supabase-uid> [--sqlite data/jobs.db] [--data-dir data] [--dry-run]

Flags:
  --user-id       Target Supabase user id stamped on every row (env: TARGET_USER_ID)
  --sqlite        Source SQLite path (default data/jobs.db)
  --data-dir      Root data dir whose profile assets move to the tenant dir (default data)
  --database-url  Target Postgres DSN (env: DATABASE_URL; required unless --dry-run)
  --dry-run       Report per-table row counts and planned asset moves, write nothing

The live database is inside the Docker volume; copy it out first:
  docker cp job-search-automation-go-backend-1:/app/db/jobs.db /tmp/jobs.db
  DATABASE_URL='postgres://...@<proj>.supabase.co:6543/postgres?sslmode=require' \
    migrate-local --sqlite /tmp/jobs.db --user-id <supabase-uid> --dry-run
`
