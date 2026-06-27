package db

import (
	"context"
	"testing"
)

// TestDeleteAuthUserGuards covers the pre-flight guards that must reject before any
// connection is attempted: an empty or LocalUser id (which would target the wrong
// row or make no sense), and a missing admin DSN. The happy path needs the auth
// schema of a live Supabase Postgres and is exercised manually / in staging.
func TestDeleteAuthUserGuards(t *testing.T) {
	cases := []struct {
		name, dsn, uid string
	}{
		{"empty uid", "postgres://x", ""},
		{"local uid", "postgres://x", LocalUser},
		{"no dsn", "", "real-uid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := DeleteAuthUser(context.Background(), c.dsn, c.uid); err == nil {
				t.Fatalf("DeleteAuthUser(%q, %q) = nil, want error", c.dsn, c.uid)
			}
		})
	}
}
