package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestMigrateAgainstLiveCopy runs the migrator against a copy of a real Node
// database to prove the legacy-baseline stamping is non-destructive: row counts
// and the table set must be unchanged, and goose must record version 1 without
// replaying DDL.
//
// Gated on JSA_LIVE_DB_COPY (path to a *copy* of jobs.db). The test copies that
// file again into a temp dir before opening, so the artifact is never mutated and
// the test is re-runnable. Skipped when the env var is unset.
func TestMigrateAgainstLiveCopy(t *testing.T) {
	src := os.Getenv("JSA_LIVE_DB_COPY")
	if src == "" {
		t.Skip("set JSA_LIVE_DB_COPY to a copy of a real jobs.db to run this check")
	}

	work := filepath.Join(t.TempDir(), "jobs.db")
	copyFile(t, src, work)

	before := snapshotDB(t, work)
	if _, ok := before.tables["jobs"]; !ok {
		t.Fatalf("source %s has no jobs table; not a real DB", src)
	}
	if _, ok := before.tables["goose_db_version"]; ok {
		t.Skip("source is already goose-managed; legacy-stamp path not exercised")
	}

	repo, err := Open(work)
	if err != nil {
		t.Fatalf("Open live copy: %v", err)
	}
	t.Cleanup(func() { repo.Close() })

	after := snapshotDB(t, work)

	// 1. Row count in jobs is untouched.
	if before.jobsRows != after.jobsRows {
		t.Fatalf("jobs row count changed: before=%d after=%d", before.jobsRows, after.jobsRows)
	}

	// 2. The only new table is goose_db_version; nothing else created or dropped.
	delete(after.tables, "goose_db_version")
	if diff := tableSetDiff(before.tables, after.tables); diff != "" {
		t.Fatalf("table set changed (baseline must not replay on a live DB): %s", diff)
	}

	// 3. goose recorded the baseline as version 1.
	var version int
	if err := repo.db.QueryRow("SELECT MAX(version_id) FROM goose_db_version").Scan(&version); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	if version != 1 {
		t.Fatalf("goose version = %d, want 1", version)
	}

	// 4. Re-opening is a no-op (idempotent migrate).
	repo2, err := Open(work)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	repo2.Close()
	again := snapshotDB(t, work)
	if again.jobsRows != before.jobsRows {
		t.Fatalf("second open changed row count: %d -> %d", before.jobsRows, again.jobsRows)
	}
}

type dbSnapshot struct {
	tables   map[string]struct{}
	jobsRows int
}

func snapshotDB(t *testing.T, path string) dbSnapshot {
	t.Helper()
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer conn.Close()

	rows, err := conn.Query("SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()

	tables := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		tables[name] = struct{}{}
	}

	var jobsRows int
	if _, ok := tables["jobs"]; ok {
		if err := conn.QueryRow("SELECT COUNT(*) FROM jobs").Scan(&jobsRows); err != nil {
			t.Fatalf("count jobs: %v", err)
		}
	}
	return dbSnapshot{tables: tables, jobsRows: jobsRows}
}

func tableSetDiff(before, after map[string]struct{}) string {
	var added, removed []string
	for name := range after {
		if _, ok := before[name]; !ok {
			added = append(added, name)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			removed = append(removed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	if len(added) == 0 && len(removed) == 0 {
		return ""
	}
	return "added=" + join(added) + " removed=" + join(removed)
}

func join(s []string) string {
	out := "["
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out + "]"
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read source %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write copy: %v", err)
	}
}
