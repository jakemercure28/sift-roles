package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"job-search-automation/internal/db"
	"job-search-automation/internal/scraper"
)

// fakeWorker stands in for the scraper service, recording the profileDir of every
// /scrape call so a test can assert which tenant dirs were scraped.
func fakeWorker(t *testing.T, dirs *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ProfileDir string `json:"profileDir"`
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		*dirs = append(*dirs, body.ProfileDir)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":0,"platforms":[],"jobs":[]}`))
	}))
}

// TestRunOnceSelfHostSingleTenant guards the self-host invariant: on SQLite the
// fan-out must collapse to exactly one scrape against the unchanged root data dir,
// identical to the pre-fan-out single-tenant behavior.
func TestRunOnceSelfHostSingleTenant(t *testing.T) {
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	var gotDirs []string
	srv := fakeWorker(t, &gotDirs)
	defer srv.Close()

	const dataDir = "/app/data"
	s := New(scraper.New(srv.URL, 5*time.Second), repo, dataDir, "@daily", nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(gotDirs) != 1 {
		t.Fatalf("expected exactly 1 scrape call, got %d (%v)", len(gotDirs), gotDirs)
	}
	if gotDirs[0] != dataDir {
		t.Fatalf("profileDir = %q, want %q (self-host root)", gotDirs[0], dataDir)
	}
}

// TestRunOnceForUserDiscoversBeforeScrape guards the bootstrap fix: the
// user-triggered path must run the discovery hook before the scrape, so a freshly
// onboarded tenant gets companies discovered ahead of its first scrape (otherwise
// it scrapes empty lists, never gets rows, and never enters the cron fan-out).
func TestRunOnceForUserDiscoversBeforeScrape(t *testing.T) {
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	var gotDirs []string
	srv := fakeWorker(t, &gotDirs)
	defer srv.Close()

	s := New(scraper.New(srv.URL, 5*time.Second), repo, "/app/data", "@daily", nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	discovered := false
	s.SetDiscoveryHook(func(ctx context.Context, r *db.Repository) error {
		if len(gotDirs) != 0 {
			t.Errorf("discovery ran after the scrape; gotDirs=%v", gotDirs)
		}
		discovered = true
		return nil
	})

	if _, err := s.RunOnceForUser(context.Background(), db.LocalUser); err != nil {
		t.Fatalf("RunOnceForUser: %v", err)
	}
	if !discovered {
		t.Fatal("discovery hook was not invoked")
	}
	if len(gotDirs) != 1 {
		t.Fatalf("expected exactly 1 scrape call, got %d (%v)", len(gotDirs), gotDirs)
	}
}

func TestRunOnceForUserSeedsGlobalJobsBeforeDiscovery(t *testing.T) {
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()
	if _, err := repo.RawDB().Exec(`
		INSERT INTO global_jobs
		  (id, title, company, url, platform, location, posted_at, scraped_at, description, description_hash, first_seen_by)
		VALUES
		  ('g1', 'Platform Engineer', 'Acme', 'https://example.com/g1', 'greenhouse', 'Remote', '2026-06-01', '2026-06-02T00:00:00Z', 'Build infra.', 'h1', 'tenant-a')`); err != nil {
		t.Fatalf("seed global job: %v", err)
	}

	var gotDirs []string
	srv := fakeWorker(t, &gotDirs)
	defer srv.Close()

	s := New(scraper.New(srv.URL, 5*time.Second), repo, "/app/data", "@daily", nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetGlobalJobSeedLimit(10)

	s.SetDiscoveryHook(func(ctx context.Context, r *db.Repository) error {
		n, err := r.CountUnscored()
		if err != nil {
			t.Fatalf("CountUnscored: %v", err)
		}
		if n != 1 {
			t.Fatalf("global seed should run before discovery; unscored=%d, want 1", n)
		}
		return nil
	})

	if _, err := s.RunOnceForUser(context.Background(), db.LocalUser); err != nil {
		t.Fatalf("RunOnceForUser: %v", err)
	}
}

// TestRunOnceForUserScrapesWhenDiscoveryFails guards the best-effort contract: a
// discovery failure is logged but must not abort the scrape.
func TestRunOnceForUserScrapesWhenDiscoveryFails(t *testing.T) {
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	var gotDirs []string
	srv := fakeWorker(t, &gotDirs)
	defer srv.Close()

	s := New(scraper.New(srv.URL, 5*time.Second), repo, "/app/data", "@daily", nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetDiscoveryHook(func(ctx context.Context, r *db.Repository) error {
		return errors.New("discovery boom")
	})

	if _, err := s.RunOnceForUser(context.Background(), db.LocalUser); err != nil {
		t.Fatalf("RunOnceForUser: %v", err)
	}
	if len(gotDirs) != 1 {
		t.Fatalf("scrape should still run after discovery failure, got %d calls", len(gotDirs))
	}
}
