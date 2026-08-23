package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
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
	s.SetGlobalJobSeedLimit(10, 0)

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

// TestRunOnceForUserBurstRunsAndJoins guards the onboarding burst contract: the
// burst hook receives the seed keywords, runs during the cycle (concurrently
// with discovery/scrape), and is joined before RunOnceForUser returns so the
// caller's deferred context cancel can't kill it mid-call.
func TestRunOnceForUserBurstRunsAndJoins(t *testing.T) {
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	var gotDirs []string
	srv := fakeWorker(t, &gotDirs)
	defer srv.Close()

	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "companies.json"),
		[]byte(`{"SEARCH_TERMS":["marketing manager"]}`), 0o644); err != nil {
		t.Fatalf("write companies.json: %v", err)
	}

	s := New(scraper.New(srv.URL, 5*time.Second), repo, dataDir, "@daily", nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	var (
		burstDone atomic.Bool
		gotKW     []string
	)
	s.SetOnboardBurstHook(func(ctx context.Context, r *db.Repository, kw []string) error {
		gotKW = kw
		time.Sleep(50 * time.Millisecond) // outlive the (instant) fake scrape
		burstDone.Store(true)
		return nil
	})

	if _, err := s.RunOnceForUser(context.Background(), db.LocalUser); err != nil {
		t.Fatalf("RunOnceForUser: %v", err)
	}
	if !burstDone.Load() {
		t.Fatal("RunOnceForUser returned before the burst finished (join missing)")
	}
	if len(gotKW) == 0 || gotKW[0] != "marketing" {
		t.Fatalf("burst keywords = %v, want tokens from companies.json SEARCH_TERMS", gotKW)
	}
	if len(gotDirs) != 1 {
		t.Fatalf("expected exactly 1 scrape call, got %d", len(gotDirs))
	}
}

// TestRunOnceForUserBurstErrorDoesNotFailCycle guards the best-effort contract
// for the burst, mirroring the discovery-failure test.
func TestRunOnceForUserBurstErrorDoesNotFailCycle(t *testing.T) {
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
	s.SetOnboardBurstHook(func(ctx context.Context, r *db.Repository, kw []string) error {
		return errors.New("burst boom")
	})

	if _, err := s.RunOnceForUser(context.Background(), db.LocalUser); err != nil {
		t.Fatalf("RunOnceForUser: %v", err)
	}
	if len(gotDirs) != 1 {
		t.Fatalf("scrape should still run after burst failure, got %d calls", len(gotDirs))
	}
}

// TestPostScrapeScoringHookRuns guards the scrape→score coupling on both paths:
// each successful tenant scrape must be followed by one scoring-hook call.
func TestPostScrapeScoringHookRuns(t *testing.T) {
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

	scoreCalls := 0
	s.SetScoringHook(func(ctx context.Context, r *db.Repository) error {
		if len(gotDirs) != scoreCalls+1 {
			t.Errorf("scoring hook ran before its scrape; scrapes=%d calls=%d", len(gotDirs), scoreCalls)
		}
		scoreCalls++
		return nil
	})

	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if scoreCalls != 1 {
		t.Fatalf("scoring hook calls after RunOnce = %d, want 1", scoreCalls)
	}

	if _, err := s.RunOnceForUser(context.Background(), db.LocalUser); err != nil {
		t.Fatalf("RunOnceForUser: %v", err)
	}
	if scoreCalls != 2 {
		t.Fatalf("scoring hook calls after RunOnceForUser = %d, want 2", scoreCalls)
	}
}

// TestPostScrapeScoringErrorDoesNotFailCycle: the chained pass is best-effort.
func TestPostScrapeScoringErrorDoesNotFailCycle(t *testing.T) {
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
	s.SetScoringHook(func(ctx context.Context, r *db.Repository) error {
		return errors.New("scoring boom")
	})

	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
}

func TestBoardScrapeDeadline(t *testing.T) {
	now := time.Date(2026, 7, 8, 15, 0, 0, 0, time.UTC)
	cycleDL := now.Add(60 * time.Minute)

	// Normal: 15m reserve leaves the board scrape a deadline 15m before cycle end.
	if bd, ok := boardScrapeDeadline(cycleDL, 15*time.Minute, now); !ok || !bd.Equal(now.Add(45*time.Minute)) {
		t.Errorf("reserve 15m: got (%v, %v), want (%v, true)", bd, ok, now.Add(45*time.Minute))
	}
	// Disabled: reserve 0 -> do not bound.
	if _, ok := boardScrapeDeadline(cycleDL, 0, now); ok {
		t.Error("reserve 0 should not bound the board scrape")
	}
	// Reserve >= remaining budget: don't bound (would leave board scrape no time).
	if _, ok := boardScrapeDeadline(now.Add(10*time.Minute), 15*time.Minute, now); ok {
		t.Error("reserve exceeding remaining budget should not bound the board scrape")
	}
}
