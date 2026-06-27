package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// DeleteAuthUser removes the Supabase GoTrue auth.users row for uid using an
// admin (non-RLS) Postgres connection. The restricted role the dashboard's serving
// connection uses has no rights on the auth schema, so account deletion's auth half
// must run through the privileged DATABASE_URL. Deleting the row cascades within the
// auth schema to identities, sessions, and refresh tokens, so the login is truly
// gone — not just the tenant's public-schema data.
//
// This is the companion to DeleteTenant: handleDeleteAccount wipes the tenant's rows
// first, then calls this so a "deleted" account can't sign back in to a fresh empty
// tenant. It opens a short-lived single connection rather than holding an admin pool,
// keeping the privileged DSN out of the steady request-serving path. adminDSN is the
// privileged DATABASE_URL; uid must be a real hosted user id.
func DeleteAuthUser(ctx context.Context, adminDSN, uid string) error {
	if uid == "" || uid == LocalUser {
		return fmt.Errorf("DeleteAuthUser: refusing to delete with empty/local user id")
	}
	if adminDSN == "" {
		return fmt.Errorf("DeleteAuthUser: no admin DSN configured")
	}
	connConfig, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		return fmt.Errorf("parse admin dsn: %w", err)
	}
	// Transaction-pooler safety, matching OpenPostgres: no server-side prepares.
	connConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	name := stdlib.RegisterConnConfig(connConfig)
	defer stdlib.UnregisterConnConfig(name)

	sqldb, err := sql.Open("pgx", name)
	if err != nil {
		return err
	}
	defer sqldb.Close()
	sqldb.SetMaxOpenConns(1)

	res, err := sqldb.ExecContext(ctx, "DELETE FROM auth.users WHERE id = $1", uid)
	if err != nil {
		return fmt.Errorf("delete auth user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Already gone (e.g. a retried request after a partial delete). Not an error:
		// the desired end state — no auth.users row for uid — already holds.
		return nil
	}
	return nil
}
