package db

import (
	"database/sql"
	"embed"
	"fmt"
	"strconv"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/sqlite/*.sql migrations/postgres/*.sql
var migrationsFS embed.FS

// Per-backend migration tracks inside migrationsFS that goose reads.
const (
	sqliteMigrationsDir   = "migrations/sqlite"
	postgresMigrationsDir = "migrations/postgres"
)

// legacySchemaVersion is the in-code Node schema version (lib/db/schema.js
// MIGRATIONS length) at which the database already contains the full schema the
// squashed baseline (00001_baseline.sql) describes. A live DB at or above this
// version is stamped as already-baselined instead of having the baseline replayed.
const legacySchemaVersion = 27

// Migrate brings the schema up to date with the embedded goose migrations.
//
// Three cases:
//   - Fresh DB: goose runs 00001_baseline.sql to build the whole schema.
//   - Legacy Node DB (no goose table, metadata.schema_version >= 27): the baseline
//     is stamped as applied without replay, leaving existing data untouched.
//   - Already goose-managed: goose applies any migrations newer than the recorded
//     version.
func Migrate(sqldb *sql.DB, dl dialect) error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())

	// Postgres is always a fresh hosted DB (no legacy Node baseline to stamp):
	// goose builds the whole schema from the Postgres migration track.
	if dl.kind == Postgres {
		if err := goose.SetDialect("postgres"); err != nil {
			return fmt.Errorf("goose dialect: %w", err)
		}
		if err := goose.Up(sqldb, postgresMigrationsDir); err != nil {
			return fmt.Errorf("goose up: %w", err)
		}
		return nil
	}

	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}

	if err := stampLegacyBaseline(sqldb); err != nil {
		return fmt.Errorf("stamp legacy baseline: %w", err)
	}

	if err := goose.Up(sqldb, sqliteMigrationsDir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

// stampLegacyBaseline records the baseline migration as already-applied on a
// database that predates goose but is already at the full in-code schema, so
// goose.Up does not re-run DDL against live tables. It is a no-op for fresh
// databases (goose builds them normally) and for databases goose already manages.
func stampLegacyBaseline(sqldb *sql.DB) error {
	managed, err := tableExists(sqldb, "goose_db_version")
	if err != nil {
		return err
	}
	if managed {
		return nil // goose already owns versioning
	}

	hasJobs, err := tableExists(sqldb, "jobs")
	if err != nil {
		return err
	}
	if !hasJobs {
		return nil // fresh DB: let goose.Up run the baseline
	}

	if version := legacyNodeSchemaVersion(sqldb); version < legacySchemaVersion {
		// jobs exists but the DB predates the full schema. The baseline uses
		// IF NOT EXISTS, so letting goose.Up run it safely fills in any missing
		// tables/indexes without touching existing ones.
		return nil
	}

	// Live DB already at the full schema: create goose's version table and mark
	// the baseline applied so goose.Up treats version 1 as done.
	if err := createGooseVersionTable(sqldb); err != nil {
		return err
	}
	// version_id 0 is goose's initial sentinel row; 1 is our baseline.
	_, err = sqldb.Exec(
		"INSERT INTO goose_db_version (version_id, is_applied) VALUES (0, 1), (1, 1)",
	)
	return err
}

// legacyNodeSchemaVersion reads metadata.schema_version written by the Node
// migration system. Returns 0 if absent or unparseable.
func legacyNodeSchemaVersion(sqldb *sql.DB) int {
	var raw sql.NullString
	if err := sqldb.QueryRow(
		"SELECT value FROM metadata WHERE key = 'schema_version'",
	).Scan(&raw); err != nil || !raw.Valid {
		return 0
	}
	v, err := strconv.Atoi(raw.String)
	if err != nil {
		return 0
	}
	return v
}

func tableExists(sqldb *sql.DB, name string) (bool, error) {
	var found string
	err := sqldb.QueryRow(
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", name,
	).Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// createGooseVersionTable mirrors the goose_db_version schema goose creates for
// the sqlite3 dialect, so a pre-existing legacy DB can be stamped before goose.Up
// runs (goose.Up then sees version 1 as already applied).
func createGooseVersionTable(sqldb *sql.DB) error {
	_, err := sqldb.Exec(`
		CREATE TABLE IF NOT EXISTS goose_db_version (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			version_id INTEGER NOT NULL,
			is_applied INTEGER NOT NULL,
			tstamp     TIMESTAMP DEFAULT (datetime('now'))
		)`)
	return err
}
