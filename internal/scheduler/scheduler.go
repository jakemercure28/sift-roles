// Package scheduler runs the scrape→insert cycle: it asks the TypeScript worker
// for leads and writes the new ones into the shared database. The Node scorer
// (unchanged) picks up the resulting pending/unscored rows.
package scheduler

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"

	"job-search-automation/internal/db"
	"job-search-automation/internal/metrics"
	"job-search-automation/internal/middleware"
	"job-search-automation/internal/scraper"
)

// Scheduler wires the worker client and the repository on a cron schedule.
type Scheduler struct {
	client    *scraper.Client
	repo      *db.Repository
	dataDir   string
	schedule  string
	platforms []string
	log       *slog.Logger
	// running guards against overlapping cycles (cron tick, boot scrape, and the
	// on-demand HTTP trigger all share one in-flight flag).
	running atomic.Bool
	// discover, when set, runs company discovery for a tenant before its
	// user-triggered scrape. It bootstraps a freshly onboarded tenant whose company
	// lists are still empty: the cron/maintenance path only services tenants that
	// already own job rows, so a brand-new tenant would otherwise never get
	// discovered and every scrape would find nothing. Best-effort; errors are
	// logged, not fatal.
	discover func(context.Context, *db.Repository) error
	// globalSeedLimit bounds the first-run seed from global_jobs before the slower
	// discovery/scrape path runs. 0 disables seeding.
	globalSeedLimit int
}

// Result summarizes one scrape→insert cycle.
type Result struct {
	Scraped  int
	Inserted int
}

// New builds a Scheduler. platforms nil/empty means "all platforms". dataDir is
// the configured root data dir (cfg.DataDir); each cycle resolves the active
// tenant's profile dir under it so the worker scrapes the right company config.
func New(client *scraper.Client, repo *db.Repository, dataDir, schedule string, platforms []string, log *slog.Logger) *Scheduler {
	return &Scheduler{
		client:    client,
		repo:      repo,
		dataDir:   dataDir,
		schedule:  schedule,
		platforms: platforms,
		log:       log,
	}
}

// RunOnce performs a single scrape→insert cycle for every background tenant. On
// self-host SQLite this is exactly one pass over the LocalUser repo and the root
// data dir, identical to the single-tenant behavior; on hosted Postgres it fans
// out one scrape per provisioned tenant, each against that tenant's own profile
// dir and repository so company config, rows, and heartbeat stay isolated. The
// returned Result aggregates across tenants. A per-tenant scrape failure is logged
// and does not abort the others, but the first such error is returned so the cron
// still surfaces a problem.
func (s *Scheduler) RunOnce(ctx context.Context) (Result, error) {
	// Stamp the run's trace ID (set by the HTTP trigger) onto every line of this
	// cycle so a manual scrape correlates end to end. Falls back to s.log on the
	// cron path, which has no request trace.
	log := s.log
	if traceID := middleware.TraceID(ctx); traceID != "" {
		log = s.log.With("trace_id", traceID)
	}

	// BackgroundTenants (not Tenants) so the scrape cron also services a tenant
	// that finished onboarding but has not produced job rows yet; keying on job
	// rows alone stranded such tenants out of every cron forever.
	tenants, err := s.repo.BackgroundTenants(s.dataDir)
	if err != nil {
		return Result{}, err
	}

	var total Result
	var firstErr error
	for _, uid := range tenants {
		repo := s.repo
		if uid != db.LocalUser {
			repo = s.repo.ForUser(uid)
		}
		res, err := s.scrapeTenant(ctx, repo, uid, log)
		total.Scraped += res.Scraped
		total.Inserted += res.Inserted
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return total, firstErr
}

// RunOnceForUser performs one scrape→insert cycle for a specific tenant without
// requiring that tenant to already own job rows. This is the first-run path used
// by the dashboard setup wizard: a fresh hosted tenant will not appear in
// Tenants() until after its first successful insert.
func (s *Scheduler) RunOnceForUser(ctx context.Context, uid string) (Result, error) {
	if uid == "" {
		return s.RunOnce(ctx)
	}
	repo := s.repo
	if uid != db.LocalUser {
		repo = s.repo.ForUser(uid)
	}
	log := s.log
	if traceID := middleware.TraceID(ctx); traceID != "" {
		log = s.log.With("trace_id", traceID)
	}
	if s.globalSeedLimit > 0 {
		seeded, err := repo.SeedTenantJobsFromGlobal(s.globalSeedLimit)
		if err != nil {
			log.Warn("global job seed failed", "user_id", uid, "error", err)
		} else if seeded > 0 {
			log.Info("global job seed complete", "user_id", uid, "inserted", seeded, "limit", s.globalSeedLimit)
		}
	}
	// Bootstrap step: a freshly onboarded tenant has empty company lists, so its
	// first scrape would find nothing and it would never enter Tenants() for the
	// crons (incl. discovery) to pick it up. Discover companies first so the scrape
	// below has somewhere to look. Best-effort; a failure still lets the scrape run.
	if s.discover != nil {
		if err := s.discover(ctx, repo); err != nil {
			log.Warn("pre-scrape discovery failed", "error", err)
		}
	}
	return s.scrapeTenant(ctx, repo, uid, log)
}

// SetDiscoveryHook installs the pre-scrape company-discovery bootstrap used by the
// user-triggered (setup wizard / "Scrape now") path. See the discover field.
func (s *Scheduler) SetDiscoveryHook(fn func(context.Context, *db.Repository) error) {
	s.discover = fn
}

// SetGlobalJobSeedLimit enables bounded seeding from the global job cache on the
// user-triggered scrape path (setup wizard / "Scrape now").
func (s *Scheduler) SetGlobalJobSeedLimit(n int) {
	s.globalSeedLimit = n
}

// scrapeTenant runs the scrape→insert cycle for a single tenant against its own
// repository and profile dir. New rows are inserted as pending/unscored and get a
// 'scraped' event.
func (s *Scheduler) scrapeTenant(ctx context.Context, repo *db.Repository, uid string, log *slog.Logger) (Result, error) {
	if uid != db.LocalUser {
		log = log.With("user_id", uid)
	}
	profileDir := db.TenantDataDir(s.dataDir, repo.DBType(), uid)
	start := time.Now()

	leads, err := s.client.Scrape(ctx, s.platforms, profileDir)
	if err != nil {
		if hbErr := repo.WriteHeartbeat("error", 0, 0, err.Error()); hbErr != nil {
			log.Warn("heartbeat write failed", "error", hbErr)
		}
		metrics.ObserveScrapeCycle(false, time.Since(start))
		return Result{}, err
	}

	var inserted int
	for _, lead := range leads {
		jobID := repo.JobRowID(lead.ID)
		ok, err := repo.InsertScrapedLead(lead)
		if err != nil {
			log.Error("insert failed", "id", lead.ID, "error", err)
			continue
		}
		if !ok {
			continue
		}
		inserted++
		if err := repo.LogEvent(jobID, "scraped", "", lead.ATSPlatformName); err != nil {
			log.Warn("log event failed", "id", jobID, "error", err)
		}
		log.Info("job scraped",
			"job_id", jobID,
			"global_job_id", lead.ID,
			"company", lead.Company,
			"platform", lead.ATSPlatformName,
		)
		metrics.ObserveScrapedJob(lead.ATSPlatformName)
	}

	res := Result{Scraped: len(leads), Inserted: inserted}
	if err := repo.WriteHeartbeat("ok", res.Scraped, res.Inserted, ""); err != nil {
		log.Warn("heartbeat write failed", "error", err)
	}
	metrics.ObserveScrapeCycle(true, time.Since(start))
	log.Info("scrape cycle complete", "scraped", res.Scraped, "inserted", res.Inserted)
	return res, nil
}

// RunOnceGuarded runs a scrape cycle (blocking) unless one is already in flight.
// The boolean reports whether this call actually started a cycle: false means
// another run held the flag, so this call was a no-op. Used by the cron tick and
// the boot scrape, which want to wait for the result.
func (s *Scheduler) RunOnceGuarded(ctx context.Context) (Result, bool, error) {
	if !s.running.CompareAndSwap(false, true) {
		return Result{}, false, nil
	}
	defer s.running.Store(false)
	res, err := s.RunOnce(ctx)
	return res, true, err
}

// TryStart begins a scrape cycle without blocking. If userID is non-empty, the
// cycle targets that tenant even if it has no existing job rows; otherwise it
// fans out over Tenants(), matching the cron path. If a cycle is already in
// flight it returns false; otherwise it launches the cycle in a background
// goroutine (bounded by timeout, detached from any request context) and returns
// true immediately. Used by the on-demand HTTP trigger, which must respond fast
// while a scrape may take minutes. It shares the same in-flight guard as
// RunOnceGuarded, so a manual scrape and the cron tick can never overlap.
func (s *Scheduler) TryStart(ctx context.Context, timeout time.Duration, userID string) bool {
	if !s.running.CompareAndSwap(false, true) {
		return false
	}
	baseCtx := context.Background()
	if traceID := middleware.TraceID(ctx); traceID != "" {
		baseCtx = middleware.ContextWithTraceID(baseCtx, traceID)
	}
	log := s.log
	if traceID := middleware.TraceID(baseCtx); traceID != "" {
		log = s.log.With("trace_id", traceID)
	}
	go func() {
		defer s.running.Store(false)
		ctx, cancel := context.WithTimeout(baseCtx, timeout)
		defer cancel()
		var err error
		if userID != "" {
			_, err = s.RunOnceForUser(ctx, userID)
		} else {
			_, err = s.RunOnce(ctx)
		}
		if err != nil {
			log.Error("triggered scrape failed", "error", err)
		} else {
			log.Info("triggered scrape complete")
		}
	}()
	return true
}

// Start registers the cron job and blocks until ctx is cancelled.
func (s *Scheduler) Start(ctx context.Context) error {
	c := cron.New()
	if _, err := c.AddFunc(s.schedule, func() {
		if _, started, err := s.RunOnceGuarded(ctx); err != nil {
			s.log.Error("scheduled scrape failed", "error", err)
		} else if !started {
			s.log.Info("scheduled scrape skipped: a scrape is already running")
		}
	}); err != nil {
		return err
	}
	s.log.Info("scheduler started", "schedule", s.schedule)
	c.Start()
	<-ctx.Done()
	stopCtx := c.Stop()
	<-stopCtx.Done()
	return nil
}
