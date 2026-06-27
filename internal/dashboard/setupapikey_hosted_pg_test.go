package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"job-search-automation/internal/db"
)

// TestSetupAPIKeyHostedIsNoOpNotEnv is the security check for the host-key
// footgun: in hosted (Postgres) mode every tenant scores on the shared host key
// configured at deploy, so the setup wizard's key step must be a no-op and must
// never touch the shared .env or process env. Otherwise one signup could
// overwrite the host key for everyone.
// Gated on JSA_PG_DSN like the other Postgres tests (see isolation_pg_test.go).
func TestSetupAPIKeyHostedIsNoOpNotEnv(t *testing.T) {
	dsn := os.Getenv("JSA_PG_DSN")
	if dsn == "" {
		t.Skip("set JSA_PG_DSN to a throwaway Postgres DSN to run the hosted host-key guard test")
	}
	root := t.TempDir()
	t.Chdir(root) // so any accidental .env write lands here and we can assert it didn't happen

	repo, err := db.OpenPostgres(dsn, db.DefaultPoolConfig(), "", false)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	srv, err := New(t.TempDir(), repo, nil, time.Second, 500, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	const key = "hosted-secret-key"
	postJSONStatus(t, ts.URL+"/api/setup/api-key", `{"key":"`+key+`"}`, http.StatusOK)

	// The shared .env must not carry the key, and the process env must be untouched.
	if got := readTextFileSafe(filepath.Join(root, ".env")); strings.Contains(got, key) || strings.Contains(got, "GEMINI_API_KEY=") {
		t.Fatalf("hosted setup wrote the shared .env: %q", got)
	}
	if os.Getenv("GEMINI_API_KEY") == key {
		t.Fatal("hosted setup mutated process env GEMINI_API_KEY")
	}
}
