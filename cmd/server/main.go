// Command server is the Go orchestrator for the split-stack job pipeline: it
// schedules scrapes, calls the TypeScript worker, writes scraped leads into the
// shared SQLite database, and (when enabled) scores unscored jobs via Gemini.
//
// Scheduled scoring is gated by SCORING_ENABLED and defaults OFF, so the Go
// service does not double-score while the Node worker still scores; it flips on
// at the Node cutover. The score-once subcommand always works for manual runs.
//
// Subcommands:
//
//	server                      start the scheduler (default)
//	server scrape-once [plats]  run one scrape→insert cycle, then exit
//	server scrape-test  [plats] probe the worker only (no DB writes)
//	server score-once           score unscored jobs up to the daily quota, then exit
//	server canonicalize-once    resolve alternate ATS rows into canonical primary rows
//	server maintain-once        run the DB-maintenance pass (dedup + auto-ghost), then exit
//	server discover-once        discover and verify new company ATS boards
//	server add-company ...      verify + record one company's ATS board (/add-company)
//	server voice-check "text"   voice-check an application answer (/app-questions, /apply)
//	server descriptions-once    check today's new jobs for short descriptions
//	server closed-check-once    check source ATSes for jobs that have closed
//	server slug-health-once     validate configured ATS board slugs
//	server rejection-sync-once  sync Gmail rejection emails into job stages
//	server context-update-once  write generated .context files from the DB
//	server market-research-once refresh market-research-cache.json when stale
//	server prune-logs-once      delete old dated component logs, then exit
//	server dashboard            run the dashboard front door (static + proxy to Node)
//	server queue-worker         run the isolated Go/Asynq queue worker
//	server healthcheck          Docker liveness: DB openable + worker reachable
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hibiken/asynq"
	"github.com/robfig/cron/v3"

	"job-search-automation/internal/ats"
	"job-search-automation/internal/auth"
	"job-search-automation/internal/closedcheck"
	"job-search-automation/internal/config"
	"job-search-automation/internal/contextupdate"
	"job-search-automation/internal/dashboard"
	"job-search-automation/internal/db"
	"job-search-automation/internal/discovery"
	"job-search-automation/internal/logging"
	"job-search-automation/internal/logprune"
	"job-search-automation/internal/metrics"
	"job-search-automation/internal/middleware"
	"job-search-automation/internal/pipeline"
	"job-search-automation/internal/rejectionsync"
	"job-search-automation/internal/scheduler"
	"job-search-automation/internal/scorer"
	"job-search-automation/internal/scraper"
	"job-search-automation/internal/slughealth"
	"job-search-automation/internal/tasks"
	"job-search-automation/internal/trigger"
	"job-search-automation/internal/voice"
)

func main() {
	cfg := config.Load()
	logger := logging.New(os.Stdout, logging.Options{
		Level:   logging.ParseLevel(cfg.LogLevel),
		Service: "job-search-go",
	})

	cmd := ""
	var args []string
	if len(os.Args) > 1 {
		cmd = os.Args[1]
		args = os.Args[2:]
	}

	// The healthcheck runs every minute; keep it quiet on success.
	if cmd != "healthcheck" {
		logger.Info("config loaded",
			"scraper_url", cfg.ScraperURL,
			"db_path", cfg.DBPath,
			"scrape_schedule", cfg.ScrapeSchedule,
			"canonicalize_schedule", cfg.CanonicalizeSchedule,
			"scrape_timeout", cfg.ScrapeTimeout.String(),
			"redis_addr", cfg.RedisAddr,
		)
		// One-time migration: convert a legacy data/companies.js profile into
		// companies.json before anything reads it (the worker now reads JSON).
		if migrated, err := discovery.MigrateCompaniesFile(cfg.DataDir); err != nil {
			logger.Warn("companies.js -> companies.json migration failed", "error", err)
		} else if migrated {
			logger.Info("migrated data/companies.js to companies.json")
		}
	}

	client := scraper.New(cfg.ScraperURL, cfg.ScrapeTimeout)

	switch cmd {
	case "healthcheck":
		// Docker liveness probe (distroless image has no shell/curl): confirm the
		// DB is openable and the worker is reachable. Exit 0 healthy, 1 otherwise.
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		repo, err := openRepo(cfg)
		if err != nil {
			logger.Error("healthcheck failed: database", "error", err)
			os.Exit(1)
		}
		repo.Close()
		if err := client.Health(ctx); err != nil {
			logger.Error("healthcheck failed: worker unreachable", "error", err)
			os.Exit(1)
		}

	case "scrape-test":
		// Worker-only probe: no DB needed.
		ctx, cancel := context.WithTimeout(context.Background(), cfg.ScrapeTimeout)
		defer cancel()
		if err := client.Health(ctx); err != nil {
			logger.Error("worker health check failed", "error", err)
			os.Exit(1)
		}
		// Worker-only connectivity probe: no repo/tenant, so the worker scrapes
		// against its own DATA_DIR (empty profileDir).
		leads, err := client.Scrape(ctx, args, "")
		if err != nil {
			logger.Error("scrape failed", "error", err)
			os.Exit(1)
		}
		logger.Info("scrape-test complete", "count", len(leads))

	case "scrape-once":
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		ctx, cancel := context.WithTimeout(context.Background(), cfg.ScrapeTimeout)
		defer cancel()
		sched := scheduler.New(client, repo, cfg.DataDir, cfg.ScrapeSchedule, args, logger)
		if _, err := sched.RunOnce(ctx); err != nil {
			logger.Error("scrape-once failed", "error", err)
			os.Exit(1)
		}

	case "score-once":
		// Score unscored jobs once and exit. No timeout: a full batch can take
		// many minutes at the rate limit, but it stays interruptible via signals.
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		res, err := runScoringPass(ctx, cfg, repo, logger)
		if err != nil {
			logger.Error("score-once failed", "error", err)
			os.Exit(1)
		}
		logger.Info("score-once complete", "scored_ok", res.ScoredOK, "scored_failed", res.ScoredFailed)

	case "maintain-once":
		// Run the DB-maintenance pass (dedup + auto-ghost) once and exit.
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		runMaintenance(repo, cfg, logger)

	case "dedup-once":
		// Run ONLY the dedup passes (archive re-posts/duplicates/alternates) once
		// and exit. Unlike maintain-once this touches no external APIs, so it is
		// safe to run on demand to collapse a backlog without spending quota.
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		res, err := repo.DedupExistingJobs()
		if err != nil {
			logger.Error("dedup-once failed", "error", err)
			os.Exit(1)
		}
		logger.Info("dedup-once complete",
			"reposts", res.Reposts, "pending", res.Pending, "alternates", res.Alternates, "total", res.Total())

	case "delete-tenant":
		// Permanently delete one tenant's data: every user_id-scoped DB row plus
		// their on-disk profile dir. This is how a data-deletion request is honored.
		// Usage: delete-tenant <user-id> confirm   (the literal "confirm" guards
		// against an accidental wipe). Irreversible.
		if len(args) < 2 || args[1] != "confirm" {
			fmt.Println(`Usage: delete-tenant <user-id> confirm
Permanently deletes all of a tenant's jobs, events, notes, usage, keys, and
their profile directory. Irreversible. The literal word "confirm" is required.`)
			os.Exit(2)
		}
		uid := args[0]
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		tenantRepo := repo.ForUser(uid)
		rows, err := tenantRepo.DeleteTenant()
		if err != nil {
			logger.Error("delete-tenant failed", "user_id", uid, "error", err)
			os.Exit(1)
		}
		dir := db.TenantDataDir(cfg.DataDir, tenantRepo.DBType(), uid)
		// Only remove a NESTED per-tenant dir. For SQLite/LocalUser, TenantDataDir
		// returns the base data dir itself, so removing it would wipe the whole
		// install (incl. jobs.db); the DB rows are already gone, so skip it.
		switch {
		case dir == cfg.DataDir:
			logger.Info("delete-tenant: skipping profile-dir removal (base data dir, not a per-tenant dir)", "dir", dir)
		default:
			if rmErr := os.RemoveAll(dir); rmErr != nil {
				logger.Warn("delete-tenant: DB rows deleted but profile dir removal failed",
					"user_id", uid, "dir", dir, "error", rmErr)
			}
		}
		logger.Info("delete-tenant complete", "user_id", uid, "rows_deleted", rows, "profile_dir", dir)

	case "discover-once":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		if _, err := runDiscovery(ctx, cfg, repo, logger); err != nil {
			logger.Error("discover-once failed", "error", err)
			os.Exit(1)
		}

	case "add-company":
		// Verify and record ONE company's ATS board (the /add-company command).
		// Usage: add-company --name "<Company>" (--url <board-url> | --slug <slug>
		// [--platform greenhouse|ashby|lever]). Exit 0 added/already, 1 not
		// verified, 2 bad usage.
		ca := parseAddCompanyArgs(args)
		if ca.help {
			fmt.Println(`Usage: add-company --name "<Company>" (--url <board-url> | --slug <slug> [--platform greenhouse|ashby|lever])`)
			return
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		// Hosted Postgres keeps the company list in the tenant's profile dir, where
		// the scraper reads it; write the new board there so it is picked up.
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		res, err := discovery.AddCompany(ctx, repo.ProfileDir(cfg.DataDir), nil, ca.args)
		if err != nil {
			fmt.Fprintf(os.Stderr, "add-company: unexpected error: %v\n", err)
			os.Exit(1)
		}
		if !res.OK {
			fmt.Fprintf(os.Stderr, "add-company: %s\n", res.Reason)
			os.Exit(res.Code)
		}
		if res.Already {
			fmt.Printf("Already tracked: %s (%s:%s) — no change.\n", res.Name, res.Platform, res.ID)
		} else {
			fmt.Printf("Added: %s (%s:%s). It will be scraped on the next pipeline run.\n", res.Name, res.Platform, res.ID)
		}

	case "voice-check":
		// Voice-check an application answer (the /app-questions + /apply gate).
		// Text comes from the first arg or stdin. Exits 1 if the check fails.
		text := ""
		if len(args) > 0 {
			text = args[0]
		} else {
			raw, _ := io.ReadAll(os.Stdin)
			text = strings.TrimSpace(string(raw))
		}
		if text == "" {
			fmt.Fprintln(os.Stderr, `Usage: voice-check "text to check"`)
			os.Exit(1)
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		res := voice.CheckVoiceText(ctx, nil, text, os.Getenv("SAPLING_API_KEY"), os.Getenv("HUGGINGFACE_API_KEY"))
		printVoiceCheck(res)
		if !res.Passed {
			os.Exit(1)
		}

	case "descriptions-once":
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		if _, err := runDescriptionCheck(repo, cfg, logger); err != nil {
			logger.Error("descriptions-once failed", "error", err)
			os.Exit(1)
		}

	case "closed-check-once":
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if _, err := runClosedCheck(ctx, repo, logger); err != nil {
			logger.Error("closed-check-once failed", "error", err)
			os.Exit(1)
		}

	case "slug-health-once":
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if _, err := runSlugHealth(ctx, cfg, repo, logger); err != nil {
			logger.Error("slug-health-once failed", "error", err)
			os.Exit(1)
		}

	case "rejection-sync-once":
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if _, err := runRejectionSync(ctx, cfg, repo, logger); err != nil {
			logger.Error("rejection-sync-once failed", "error", err)
			os.Exit(1)
		}

	case "context-update-once":
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if _, err := runContextUpdate(ctx, cfg, repo, logger); err != nil {
			logger.Error("context-update-once failed", "error", err)
			os.Exit(1)
		}

	case "market-research-once":
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if _, err := runMarketResearch(ctx, cfg, repo, logger, false); err != nil {
			logger.Error("market-research-once failed", "error", err)
			os.Exit(1)
		}

	case "prune-logs-once":
		if err := runLogPrune(cfg, logger); err != nil {
			logger.Error("prune-logs-once failed", "error", err)
			os.Exit(1)
		}

	case "canonicalize-once":
		// Resolve pending alternate ATS rows into canonical primary ATS rows once
		// and exit.
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if _, err := runCanonicalize(ctx, cfg, repo, logger); err != nil {
			logger.Error("canonicalize-once failed", "error", err)
			os.Exit(1)
		}

	case "dashboard":
		// Dashboard only (no schedulers). The default `server` command serves the
		// dashboard alongside the crons; this subcommand is for running it alone.
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		rej := buildScorer(cfg, repo, logger)
		srv, err := dashboard.New(cfg.PublicDir, dashboardRepo(cfg, repo, logger), rej,
			cfg.GeminiRateDelay, cfg.GeminiDailyLimit, cfg.DataDir, logger)
		if err != nil {
			logger.Error("dashboard setup failed", "error", err)
			os.Exit(1)
		}
		srv.SetScrapeTriggerURL(cfg.ScrapeTriggerURL)
		srv.SetScrapeSchedule(cfg.ScrapeSchedule)
		srv.SetAuth(buildDashboardAuth(ctx, cfg, logger), cfg.SupabaseURL, cfg.SupabaseAnonKey)
		srv.SetAuthAdminDSN(cfg.DatabaseURL)
		srv.SetRateLimit(cfg.RateLimitPerMinute, cfg.RateLimitBurst)
		// Fresh config per run so Sync now picks up credentials saved in the
		// Settings modal without a restart.
		srv.SetRejectionSyncRunner(func(runCtx context.Context) (rejectionsync.Summary, error) {
			return runRejectionSync(runCtx, config.Load(), repo, logger)
		})
		if err := srv.Listen(ctx, cfg.DashboardAddr); err != nil {
			logger.Error("dashboard server error", "error", err)
			os.Exit(1)
		}

	case "queue-worker":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := runQueueWorker(ctx, cfg, logger); err != nil {
			logger.Error("queue worker error", "error", err)
			os.Exit(1)
		}

	default:
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		sched := scheduler.New(client, repo, cfg.DataDir, cfg.ScrapeSchedule, nil, logger)
		sched.SetGlobalJobSeedLimit(cfg.GlobalJobSeedLimit)
		// Bootstrap discovery for the user-triggered scrape path (setup wizard /
		// "Scrape now"). A freshly onboarded tenant has empty company lists and so
		// owns no job rows, which keeps it out of Tenants() and the maintenance cron
		// that normally runs discovery: without this it could never get its first
		// jobs. Only run when the tenant has no companies yet; established tenants are
		// served by the cron, so we skip them to avoid extra Gemini calls.
		sched.SetDiscoveryHook(func(ctx context.Context, r *db.Repository) error {
			if cc, err := discovery.LoadCompanyConfig(tenantProfileDir(cfg, r)); err == nil && cc.HasCompanies() {
				return nil
			}
			_, err := runDiscovery(ctx, cfg, r, logger)
			return err
		})

		// On-demand trigger: the dashboard's "Scrape now" button posts here to kick
		// off a scrape. It shares the scheduler's in-flight guard, so manual and
		// cron runs can't overlap. Reachable only inside the compose network.
		trig := trigger.New(sched, cfg.ScrapeTimeout, logger)
		go func() {
			if err := trig.Listen(ctx, cfg.TriggerAddr); err != nil {
				logger.Error("trigger server error", "error", err)
			}
		}()

		// Prometheus /metrics on a dedicated listener (not the dashboard mux), so
		// metrics are scrapeable over a loopback-published port but never exposed
		// through the public tunnel. A background refresher keeps the backlog and
		// quota gauges current between scrape/score passes.
		go func() {
			if err := metrics.Serve(ctx, cfg.MetricsAddr, logger); err != nil {
				logger.Error("metrics server error", "error", err)
			}
		}()
		go refreshMetricsGauges(ctx, cfg, repo, logger)

		// Dashboard: served by this same process now that the Node dashboard is
		// retired. "Scrape now" posts to the in-process trigger above.
		dash, err := dashboard.New(cfg.PublicDir, dashboardRepo(cfg, repo, logger), buildScorer(cfg, repo, logger),
			cfg.GeminiRateDelay, cfg.GeminiDailyLimit, cfg.DataDir, logger)
		if err != nil {
			logger.Error("dashboard setup failed", "error", err)
			os.Exit(1)
		}
		dash.SetScrapeTriggerURL(cfg.ScrapeTriggerURL)
		dash.SetScrapeSchedule(cfg.ScrapeSchedule)
		dash.SetHostKeyConfigured(cfg.GeminiAPIKey != "")
		dash.SetAuth(buildDashboardAuth(ctx, cfg, logger), cfg.SupabaseURL, cfg.SupabaseAnonKey)
		dash.SetAuthAdminDSN(cfg.DatabaseURL)
		dash.SetRateLimit(cfg.RateLimitPerMinute, cfg.RateLimitBurst)
		// Fresh config per run so Sync now picks up credentials saved in the
		// Settings modal without a restart.
		dash.SetRejectionSyncRunner(func(runCtx context.Context) (rejectionsync.Summary, error) {
			return runRejectionSync(runCtx, config.Load(), repo, logger)
		})
		go func() {
			if err := dash.Listen(ctx, cfg.DashboardAddr); err != nil {
				logger.Error("dashboard server error", "error", err)
			}
		}()

		// Scheduled scoring runs only when explicitly enabled.
		if cfg.ScoringEnabled {
			startScoringCron(ctx, cfg, repo, logger)
		}

		// DB maintenance (dedup + auto-ghost), ported from the Node worker.
		startMaintenanceCron(ctx, cfg, repo, logger)
		startCanonicalizeCron(ctx, cfg, repo, logger)
		// Market research warms once a day on its own schedule (see comment in
		// runMaintenance): refreshing it loads every job's description, so it must not
		// ride the 30-minute maintenance fan-out.
		startMarketResearchCron(ctx, cfg, repo, logger)

		if cfg.ScrapeOnStart {
			runCtx, cancel := context.WithTimeout(ctx, cfg.ScrapeTimeout)
			if _, _, err := sched.RunOnceGuarded(runCtx); err != nil {
				logger.Error("startup scrape failed", "error", err)
			}
			cancel()
		}
		// Score the backlog on startup (after the optional scrape) so a fresh
		// deploy surfaces freshly-scraped jobs without waiting for the scoring
		// cron. Gated on ScoringEnabled so it never runs when scoring is off.
		if cfg.ScoringEnabled && cfg.ScoreOnStart {
			forEachTenant(repo, cfg.DataDir, logger, func(tr *db.Repository) {
				res, err := runScoringPass(ctx, cfg, tr, logger)
				if err != nil {
					logger.Error("startup scoring failed", "user_id", tr.UserID(), "error", err)
				} else {
					logger.Info("startup scoring complete", "user_id", tr.UserID(), "scored_ok", res.ScoredOK, "scored_failed", res.ScoredFailed)
				}
			})
		}
		// Keep the Market Research body cache hot in the background (this same
		// process serves the dashboard), so visits skip the cold rebuild that
		// re-fetches every job. Launched after the optional startup scrape/scoring
		// so the first warm renders settled data.
		go warmMarketResearchLoop(ctx, dash, repo, cfg.DataDir, logger)
		if err := sched.Start(ctx); err != nil {
			logger.Error("scheduler error", "error", err)
			os.Exit(1)
		}
	}
}

func runQueueWorker(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	redis := asynq.RedisClientOpt{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
	}
	server := asynq.NewServer(redis, asynq.Config{
		Concurrency: cfg.GoQueueConcurrency,
		Queues: map[string]int{
			tasks.QueueGoMaintenance: 1,
		},
	})
	mux := asynq.NewServeMux()
	tasks.RegisterHandlers(mux, logger)

	if err := server.Start(mux); err != nil {
		return err
	}
	logger.Info("queue worker started",
		"queue", tasks.QueueGoMaintenance,
		"concurrency", cfg.GoQueueConcurrency,
	)
	<-ctx.Done()
	server.Shutdown()
	return nil
}

// buildScorer wires a Gemini client (with the repo as its usage recorder) into a
// Scorer that reads resume.md from the data dir.
// printVoiceCheck renders a voice.Result the way scripts/check-voice.js did.
func printVoiceCheck(res voice.Result) {
	fmt.Println("\nVoice check")
	fmt.Println(strings.Repeat("=", 50))

	if len(res.Issues) == 0 {
		fmt.Println("Local:   no violations found")
	} else {
		fmt.Printf("Local:   %d issue(s) found\n", len(res.Issues))
		labels := map[string]string{
			"kill_word":      "Kill-list word",
			"dash":           "Dash connector",
			"banned_opener":  "Banned opener",
			"low_burstiness": "Low burstiness",
		}
		for _, issue := range res.Issues {
			label := labels[issue.Type]
			if label == "" {
				label = issue.Type
			}
			fmt.Printf("  [%s] %s\n", label, issue.Detail)
		}
	}

	switch {
	case res.Sapling == nil:
		fmt.Println("Sapling: no API key set (add SAPLING_API_KEY to .env)")
	case res.Sapling.Err != "":
		fmt.Printf("Sapling: error — %s\n", res.Sapling.Err)
	default:
		fmt.Printf("Sapling: %s\n", voice.RenderScore(res.Sapling.Score))
		flagged := make([]voice.SentenceScore, 0, len(res.Sapling.SentenceScores))
		for _, s := range res.Sapling.SentenceScores {
			if s.Score > 0.6 {
				flagged = append(flagged, s)
			}
		}
		sort.Slice(flagged, func(i, j int) bool { return flagged[i].Score > flagged[j].Score })
		if len(flagged) > 3 {
			flagged = flagged[:3]
		}
		if len(flagged) > 0 {
			fmt.Println("\nFlagged sentences:")
			for _, s := range flagged {
				snippet := s.Sentence
				if len([]rune(snippet)) > 80 {
					snippet = string([]rune(snippet)[:80]) + "..."
				}
				fmt.Printf("  %.0f%%  %q\n", s.Score*100, snippet)
			}
		} else if res.Sapling.Score >= 0.5 {
			fmt.Println("  (sentence-level model found no specific culprit — overall token statistics are flagging the text as too fluent/structured. Add more roughness: fragments, asides, informal phrasing.)")
		}
	}

	switch {
	case res.HuggingFace == nil:
		fmt.Println("HuggingFace: no API key set (add HUGGINGFACE_API_KEY to .env)")
	case res.HuggingFace.Err != "":
		fmt.Printf("HuggingFace: error — %s\n", res.HuggingFace.Err)
	default:
		fmt.Printf("HuggingFace: %s\n", voice.RenderScore(res.HuggingFace.Score))
	}

	fmt.Println("")
}

func buildScorer(cfg config.Config, repo *db.Repository, logger *slog.Logger) *scorer.Scorer {
	// Every tenant scores on the shared host key (GEMINI_API_KEY). Scope this tenant
	// in the shared per-key limiter's round-robin, and tally host-key calls globally
	// so one tenant can't drain the shared key.
	client := buildGeminiClient(cfg, repo, logger)
	// Hosted Postgres relocates the profile into the tenant dir, so the scorer must
	// read from this tenant's dir, not the root data dir. Trust the scoped repo's
	// tenant so per-tenant cron passes read each user's own dir.
	return scorer.New(client, tenantProfileDir(cfg, repo)).
		WithRescoreThreshold(cfg.ScoringRescoreThreshold)
}

func buildGeminiClient(cfg config.Config, repo *db.Repository, logger *slog.Logger) *scorer.Client {
	client := scorer.NewClient(cfg.GeminiAPIKey, cfg.GeminiRateDelay, repo, logger)
	if repo != nil {
		client = client.WithTenant(repo.UserID())
	}
	return client.WithHostKey(cfg.GeminiAPIKey != "")
}

// parseAddCompanyArgs reads the add-company subcommand flags (--name/--url/
// --slug/--platform, plus --help/-h). A bare flag with no following value (or
// followed by another flag) is treated as a boolean, matching add-company.js.
func parseAddCompanyArgs(argv []string) struct {
	args discovery.AddCompanyArgs
	help bool
} {
	var out struct {
		args discovery.AddCompanyArgs
		help bool
	}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--help" || a == "-h" {
			out.help = true
			continue
		}
		if !strings.HasPrefix(a, "--") {
			if out.args.Name == "" {
				out.args.Name = a
			}
			continue
		}
		key := strings.TrimPrefix(a, "--")
		val := ""
		if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "--") {
			val = argv[i+1]
			i++
		}
		switch key {
		case "name":
			out.args.Name = val
		case "url", "workday-url":
			out.args.URL = val
		case "slug":
			out.args.Slug = val
		case "platform":
			out.args.Platform = val
		}
	}
	return out
}

// scoreConfig builds the pipeline knobs. hostKey flags a run on the shared host
// key so ScoreUnscored also clamps the run to the host key's global daily ceiling.
func scoreConfig(cfg config.Config, hostKey bool) pipeline.ScoreConfig {
	return pipeline.ScoreConfig{
		DailyLimit:              cfg.GeminiDailyLimit,
		Concurrency:             cfg.ScoringConcurrency,
		ArchiveThreshold:        cfg.AutoArchiveThreshold,
		BatchSize:               cfg.ScoringBatchSize,
		HostKey:                 hostKey,
		HostDailyLimit:          cfg.GeminiHostDailyLimit,
		HostPerTenantDailyLimit: cfg.GeminiHostPerTenantDailyLimit,
	}
}

// runScoringPass scores a tenant's backlog with the Flash batch path.
// Centralizing it keeps the three call sites (score-once, startup, cron) identical.
func runScoringPass(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger) (pipeline.ScoreResult, error) {
	hostKey := cfg.GeminiAPIKey != ""
	return pipeline.ScoreUnscored(ctx, repo, buildScorer(cfg, repo, logger), scoreConfig(cfg, hostKey), logger)
}

// forEachTenant runs fn once per background tenant, each with a repo scoped to
// that tenant, so the scheduled crons service every provisioned user instead of
// only ActiveTenant (the dominant one). Self-host SQLite enumerates exactly
// [LocalUser] and so runs fn once against the unchanged repo — byte-for-byte the
// pre-fan-out behavior. A per-tenant failure surfaces inside fn (logged there);
// if enumeration itself fails we fall back to a single pass on the repo as-is so a
// transient error degrades to today's single-tenant behavior rather than skipping
// the whole cycle.
func forEachTenant(repo *db.Repository, dataDir string, logger *slog.Logger, fn func(repo *db.Repository)) {
	tenants, err := repo.BackgroundTenants(dataDir)
	if err != nil {
		logger.Error("tenant enumeration failed; running single pass", "error", err)
		fn(repo)
		return
	}
	for _, uid := range tenants {
		tr := repo
		if uid != db.LocalUser {
			tr = repo.ForUser(uid)
		}
		fn(tr)
	}
}

// refreshMetricsGauges periodically publishes the per-tenant unscored backlog and
// Gemini usage gauges (plus the global host-key usage), so Grafana shows live
// values even between scrape and scoring passes. It returns when ctx is cancelled.
// Reads are best-effort: a transient query error skips that gauge for the tick.
func refreshMetricsGauges(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger) {
	tick := func() {
		hostSet := false
		forEachTenant(repo, cfg.DataDir, logger, func(tr *db.Repository) {
			if n, err := tr.CountUnscored(); err == nil {
				metrics.SetJobsPending(tr.UserID(), n)
			}
			if used, err := tr.DailyAPIUsage(scorer.Model); err == nil {
				metrics.SetGeminiUsage(tr.UserID(), used)
			}
			if tokens, err := tr.DailyAPITokens(scorer.Model); err == nil {
				metrics.SetGeminiTokenUsage(tr.UserID(), tokens.Prompt, tokens.CachedPrompt, tokens.Candidates, tokens.Total)
			}
			// Host-key usage is global (keyed on the reserved host pseudo-tenant), so
			// read it once per tick rather than once per tenant.
			if !hostSet {
				if used, err := tr.HostDailyAPIUsage(scorer.Model); err == nil {
					metrics.SetGeminiHostUsage(used)
					hostSet = true
				}
				if tokens, err := tr.HostDailyAPITokens(scorer.Model); err == nil {
					metrics.SetGeminiHostTokenUsage(tokens.Prompt, tokens.CachedPrompt, tokens.Candidates, tokens.Total)
				}
			}
		})
		// Reconciliation alarm: how many onboarded tenants still own zero jobs. The
		// self-healing fan-out should drive this to 0 within a maintenance+scrape
		// cycle; a sustained nonzero means a tenant is stranded (the bootstrap
		// deadlock this metric exists to make impossible to miss again).
		if jobs, err := repo.Tenants(); err == nil {
			withJobs := make(map[string]bool, len(jobs))
			for _, uid := range jobs {
				withJobs[uid] = true
			}
			jobless := 0
			for _, uid := range db.OnboardedTenantDirs(cfg.DataDir, repo.DBType()) {
				if !withJobs[uid] {
					jobless++
				}
			}
			metrics.SetOnboardedTenantsJobless(jobless)
		}
	}
	tick()
	// Every tick fans out per tenant (CountUnscored + DailyAPIUsage) plus a
	// Tenants() scan, so this is a small but constant stream of Postgres round trips
	// over the remote pooler. These gauges only need to be roughly live for Grafana,
	// so 5 minutes is plenty and cuts the round-trip volume ~5x.
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// marketWarmInterval is how often the warm loop re-checks each tenant's Market
// Research signature. Idle ticks cost one cheap signature query per tenant; a
// full rebuild runs only after a tenant's jobs change (the daily scrape or a
// scoring pass), keeping the rendered body hot so visits skip the cold rebuild.
// 5 minutes rather than 60s: the page also rebuilds lazily on view, so the warm
// loop only saves the occasional cold visit, and a per-minute signature query per
// tenant ran 1440 times a day each for no real benefit. 5 minutes keeps the body
// warm while cutting that idle pooler chatter ~5x.
const marketWarmInterval = 5 * time.Minute

// warmMarketResearchLoop keeps each tenant's Market Research body cache hot in
// the dashboard process so a visit lands on the in-memory memo instead of the
// multi-second cold rebuild that re-fetches every job over the remote pooler.
// dash shares one market cache across its forUser clones, so warming here is what
// the HTTP path later reads. The first tick warms immediately (covers the cold
// cache after a deploy/restart); thereafter it re-warms only tenants whose data
// changed. Returns when ctx is cancelled.
func warmMarketResearchLoop(ctx context.Context, dash *dashboard.Server, repo *db.Repository, dataDir string, logger *slog.Logger) {
	tick := func() {
		forEachTenant(repo, dataDir, logger, func(tr *db.Repository) {
			dash.WarmMarketResearch(tr.UserID())
		})
	}
	tick()
	t := time.NewTicker(marketWarmInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// tenantProfileDir resolves a profile dir from an already tenant-scoped repo,
// trusting the repo's userID instead of re-resolving the dominant tenant the way
// Repository.ProfileDir does via ActiveTenant. Inside the forEachTenant fan-out the
// repo is scoped to the loop's tenant, so this returns that tenant's dir; for the
// CLI one-shots (repo scoped to ActiveTenant by openRepo) and self-host (LocalUser)
// it returns exactly what ProfileDir would, so those paths are unchanged.
func tenantProfileDir(cfg config.Config, repo *db.Repository) string {
	return db.TenantDataDir(cfg.DataDir, repo.DBType(), repo.UserID())
}

// startScoringCron schedules scoring on cfg.ScoreSchedule with a single-flight
// guard so passes never overlap, and stops the cron when ctx is cancelled. Each
// pass fans out over every tenant, rebuilding the scorer per tenant so Gemini
// usage and the shared per-key limiter's round-robin key on the right user.
func startScoringCron(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger) {
	startCron(ctx, logger, "scoring", cfg.ScoreSchedule, func() {
		forEachTenant(repo, cfg.DataDir, logger, func(tr *db.Repository) {
			if _, err := runScoringPass(ctx, cfg, tr, logger); err != nil {
				logger.Error("scheduled scoring failed", "user_id", tr.UserID(), "error", err)
			}
		})
	})
}

// startCron schedules job on the given cron schedule with a single-flight
// guard, and stops the cron when ctx is cancelled. name is used in logs
// (e.g. "scoring" -> "scheduled scoring skipped: ...").
func startCron(ctx context.Context, logger *slog.Logger, name, schedule string, job func()) {
	var running atomic.Bool

	c := cron.New()
	if _, err := c.AddFunc(schedule, func() {
		if !running.CompareAndSwap(false, true) {
			logger.Info("scheduled " + name + " skipped: a pass is already running")
			return
		}
		defer running.Store(false)
		job()
	}); err != nil {
		logger.Error(name+" scheduler setup failed", "error", err)
		return
	}

	c.Start()
	go func() {
		<-ctx.Done()
		c.Stop()
	}()
	logger.Info(name+" scheduler started", "schedule", schedule)
}

// runMaintenance runs the DB-maintenance passes once: dedup re-posts/duplicates,
// then auto-ghost stale applied jobs. Best-effort; errors are logged, not fatal.
func runMaintenance(repo *db.Repository, cfg config.Config, logger *slog.Logger) {
	if res, err := repo.DedupExistingJobs(); err != nil {
		logger.Error("dedup failed", "error", err)
	} else if res.Total() > 0 {
		logger.Info("dedup complete",
			"reposts", res.Reposts, "pending", res.Pending, "alternates", res.Alternates)
	}
	// Canary: the dedup/canonicalize passes must never leave a listing with no
	// live, scoreable row. A non-zero count means a cascade regression slipped
	// in (the class of bug that silently deleted unique jobs), so surface it
	// loudly rather than letting it go unnoticed again.
	if n, err := repo.CountCascadedArchives(); err != nil {
		logger.Warn("cascade canary check failed", "error", err)
	} else if n > 0 {
		logger.Warn("cascade canary tripped: listings auto-archived to zero scoreable rows", "groups", n)
	}
	if n, err := repo.AutoGhostStale(cfg.GhostedAfterDays); err != nil {
		logger.Error("auto-ghost failed", "error", err)
	} else if n > 0 {
		logger.Info("auto-ghost complete", "ghosted", n, "days", cfg.GhostedAfterDays)
	}
	if err := runLogPrune(cfg, logger); err != nil {
		logger.Warn("log prune failed", "error", err)
	}
	if _, err := runDescriptionCheck(repo, cfg, logger); err != nil {
		logger.Warn("description check failed", "error", err)
	}
	if _, err := runClosedCheck(context.Background(), repo, logger); err != nil {
		logger.Warn("closed check failed", "error", err)
	}
	// Market research is intentionally NOT refreshed here. It loads every live and
	// all-time job's description into memory, so refreshing it on the 30-minute
	// maintenance fan-out re-read the whole jobs table from Postgres up to 48x/day
	// per tenant and was the dominant source of Supabase egress. It now warms once
	// a day via startMarketResearchCron, and the dashboard builds it lazily on view
	// (23h cache), so the page stays fresh without the background re-reads.
	if _, err := runDiscovery(context.Background(), cfg, repo, logger); err != nil {
		logger.Warn("company discovery failed", "error", err)
	}
	if _, err := runSlugHealth(context.Background(), cfg, repo, logger); err != nil {
		logger.Warn("slug validation failed", "error", err)
	}
	if _, err := runRejectionSync(context.Background(), cfg, repo, logger); err != nil {
		logger.Warn("rejection email sync failed", "error", err)
	}
	if _, err := runContextUpdate(context.Background(), cfg, repo, logger); err != nil {
		logger.Warn("context update failed", "error", err)
	}
}

// startMaintenanceCron schedules runMaintenance on cfg.MaintenanceSchedule with a
// single-flight guard, and stops the cron when ctx is cancelled.
func startMaintenanceCron(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger) {
	startCron(ctx, logger, "maintenance", cfg.MaintenanceSchedule, func() {
		// Reload config each pass so settings saved via the dashboard (Gmail
		// credentials, pause toggle, auto-ghost window) apply on the next
		// scheduled run instead of waiting for a container restart.
		fresh := config.Load()
		forEachTenant(repo, fresh.DataDir, logger, func(tr *db.Repository) {
			runMaintenance(tr, fresh, logger)
		})
	})
}

// startMarketResearchCron warms the Market Research report on cfg.MarketResearchSchedule
// (default once a day) with a single-flight guard, fanning out per tenant. This used to
// run inside the 30-minute maintenance pass, but each refresh reads every job's
// description out of Postgres, so running it 48x/day per tenant dominated Supabase egress.
// Once a day is enough to keep the cache warm; the dashboard also builds it lazily on view.
func startMarketResearchCron(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger) {
	startCron(ctx, logger, "market-research", cfg.MarketResearchSchedule, func() {
		fresh := config.Load()
		forEachTenant(repo, fresh.DataDir, logger, func(tr *db.Repository) {
			if _, err := runMarketResearch(ctx, fresh, tr, logger, false); err != nil {
				logger.Warn("scheduled market research refresh failed", "user_id", tr.UserID(), "error", err)
			}
		})
	})
}

// runCanonicalize resolves pending alternate ATS rows once and applies the
// canonicalization/alias DB transaction for each row.
func runCanonicalize(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger) (ats.CanonicalizeReport, error) {
	var gemini ats.GeminiClient
	if cfg.GeminiAPIKey != "" {
		gemini = buildGeminiClient(cfg, repo, logger)
	}
	report, err := ats.CanonicalizeExisting(ctx, repo, ats.CanonicalizeConfig{
		OnlyPending: true,
		Concurrency: cfg.ATSConcurrency,
		Gemini:      gemini,
		Log:         logger,
	})
	if err != nil {
		return ats.CanonicalizeReport{}, err
	}
	logger.Info("canonicalize complete",
		"rows", len(report.Rows),
		"counts", report.Counts,
		"concurrency", cfg.ATSConcurrency,
		"gemini_enabled", gemini != nil,
	)
	return report, nil
}

// startCanonicalizeCron schedules runCanonicalize on cfg.CanonicalizeSchedule
// with a single-flight guard, and stops the cron when ctx is cancelled.
func startCanonicalizeCron(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger) {
	startCron(ctx, logger, "canonicalize", cfg.CanonicalizeSchedule, func() {
		forEachTenant(repo, cfg.DataDir, logger, func(tr *db.Repository) {
			if _, err := runCanonicalize(ctx, cfg, tr, logger); err != nil {
				logger.Error("scheduled canonicalize failed", "user_id", tr.UserID(), "error", err)
			}
		})
	})
}

func runLogPrune(cfg config.Config, logger *slog.Logger) error {
	res, err := logprune.Prune(logprune.Options{
		Root:          cfg.LogDir,
		RetentionDays: cfg.LogRetentionDays,
		Log:           logger,
	})
	if err != nil {
		return err
	}
	if res.Scanned > 0 || res.Deleted > 0 {
		logger.Info("log prune complete",
			"root", cfg.LogDir,
			"retention_days", cfg.LogRetentionDays,
			"scanned", res.Scanned,
			"deleted", res.Deleted,
		)
	}
	return nil
}

func runDescriptionCheck(repo *db.Repository, cfg config.Config, logger *slog.Logger) (db.DescriptionHealth, error) {
	localDate := time.Now().Format("2006-01-02")
	health, err := repo.CheckDescriptions(localDate)
	if err != nil {
		return db.DescriptionHealth{}, err
	}
	if err := writeDescriptionHealth(cfg.JDHealthPath, health); err != nil {
		return db.DescriptionHealth{}, err
	}

	status := "ok"
	if len(health.Critical) > 0 {
		status = "critical"
	} else if len(health.Warn) > 0 {
		status = "warn"
	}
	logger.Info("description check complete",
		"status", status,
		"total", health.Total,
		"critical", len(health.Critical),
		"warn", len(health.Warn),
		"ok", health.OK,
		"output", cfg.JDHealthPath,
	)
	return health, nil
}

func runClosedCheck(ctx context.Context, repo *db.Repository, logger *slog.Logger) (closedcheck.Result, error) {
	res, err := closedcheck.Run(ctx, repo, closedcheck.Config{})
	if err != nil {
		return closedcheck.Result{}, err
	}
	logger.Info("closed check complete", "checked", res.Checked, "closed", res.Closed, "skipped", res.Skipped, "errored", res.Errored)
	return res, nil
}

func runMarketResearch(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger, force bool) (dashboard.MarketResearchRefresh, error) {
	dataDir := tenantProfileDir(cfg, repo)
	srv, err := dashboard.New(cfg.PublicDir, repo, buildScorer(cfg, repo, logger),
		cfg.GeminiRateDelay, cfg.GeminiDailyLimit, dataDir, logger)
	if err != nil {
		return dashboard.MarketResearchRefresh{}, err
	}
	res, err := srv.RefreshMarketResearch(ctx, force)
	if err != nil {
		return dashboard.MarketResearchRefresh{}, err
	}
	if res.Skipped {
		logger.Info("market research refresh skipped",
			"reason", res.Reason,
			"jobs", res.JobCount,
			"cache", res.CachePath,
		)
		return res, nil
	}
	logger.Info("market research refresh complete",
		"jobs", res.JobCount,
		"cache", res.CachePath,
	)
	return res, nil
}

func runDiscovery(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger) (discovery.Report, error) {
	var gemini discovery.GeminiClient
	if cfg.GeminiAPIKey != "" {
		gemini = buildGeminiClient(cfg, repo, logger)
	}
	dcfg := discovery.Config{
		DataDir:        tenantProfileDir(cfg, repo),
		TTLHours:       cfg.DiscoveryTTLHours,
		CandidateCount: cfg.DiscoveryCandidateCount,
		Gemini:         gemini,
		Log:            logger,
	}
	// The global verified-board registry is a shared cache across tenants: a board
	// any tenant verifies is trusted (HTTP probe skipped) and harvested for reuse.
	if repo != nil {
		dcfg.Registry = repo.CompanyRegistry()
	}
	report, err := discovery.Run(ctx, dcfg)
	if err != nil {
		return discovery.Report{}, err
	}
	if report.Skipped {
		logger.Info("company discovery skipped",
			"reason", report.Reason,
			"total_suggested", report.TotalSuggested,
			"next_eligible_at", report.NextEligibleAt,
		)
		return report, nil
	}
	logger.Info("company discovery complete",
		"added", report.Added,
		"total_suggested", report.TotalSuggested,
		"passes", report.Passes,
		"next_eligible_at", report.NextEligibleAt,
	)
	if repo != nil {
		if err := repo.WriteDiscoveryReport(report.Added); err != nil {
			logger.Warn("discovery report persist failed", "error", err)
		}
	}
	return report, nil
}

func runSlugHealth(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger) (slughealth.Summary, error) {
	summary, err := slughealth.Run(ctx, slughealth.Config{
		DataDir:    tenantProfileDir(cfg, repo),
		OutputPath: cfg.SlugHealthPath,
		Log:        logger,
	})
	if err != nil {
		return slughealth.Summary{}, err
	}
	if summary.Skipped {
		logger.Info("slug validation skipped",
			"reason", summary.Reason,
			"output", cfg.SlugHealthPath,
		)
		return summary, nil
	}
	logger.Info("slug validation complete",
		"ok", summary.Total.OK,
		"empty", summary.Total.Empty,
		"broken", summary.Total.Broken,
		"blocked", summary.Total.Blocked,
		"transient", summary.Total.Transient,
		"output", cfg.SlugHealthPath,
	)
	return summary, nil
}

// rejectionSyncRunning single-flights rejection sync so the maintenance cron
// and the dashboard's Sync now button never run overlapping IMAP sweeps.
var rejectionSyncRunning atomic.Bool

func runRejectionSync(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger) (rejectionsync.Summary, error) {
	if cfg.RejectionEmailSyncDisabled {
		logger.Info("rejection email sync skipped", "reason", "disabled")
		return rejectionsync.Summary{}, nil
	}
	if cfg.GmailEmail == "" || cfg.GmailAppPassword == "" {
		logger.Info("rejection email sync skipped", "reason", "missing_gmail_credentials")
		return rejectionsync.Summary{}, nil
	}
	if !rejectionSyncRunning.CompareAndSwap(false, true) {
		return rejectionsync.Summary{}, dashboard.ErrRejectionSyncBusy
	}
	defer rejectionSyncRunning.Store(false)
	summary, err := rejectionsync.Sync(ctx, repo, rejectionsync.Config{
		Email:        cfg.GmailEmail,
		Password:     cfg.GmailAppPassword,
		LookbackDays: cfg.RejectionEmailLookbackDays,
		MaxMessages:  cfg.RejectionEmailMaxMessages,
		SkipTrash:    cfg.RejectionEmailSkipTrash,
	})
	if err != nil {
		if werr := repo.WriteRejectionSyncStatus(db.RejectionSyncStatus{
			Status: "error",
			Error:  err.Error(),
		}); werr != nil {
			logger.Warn("rejection sync status persist failed", "error", werr)
		}
		return rejectionsync.Summary{}, err
	}
	if werr := repo.WriteRejectionSyncStatus(db.RejectionSyncStatus{
		Status:     "ok",
		Fetched:    summary.Fetched,
		Candidates: summary.Candidates,
		Applied:    summary.Applied,
		Ignored:    summary.Ignored,
		Unmatched:  summary.Unmatched,
	}); werr != nil {
		logger.Warn("rejection sync status persist failed", "error", werr)
	}
	logger.Info("rejection email sync complete",
		"fetched", summary.Fetched,
		"candidates", summary.Candidates,
		"applied", summary.Applied,
		"dry_run", summary.DryRun,
		"ignored", summary.Ignored,
		"unmatched", summary.Unmatched,
	)
	return summary, nil
}

func runContextUpdate(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger) (contextupdate.Summary, error) {
	contextDir := cfg.ContextDir
	if repo.DBType() == db.Postgres && repo.UserID() != db.LocalUser {
		contextDir = filepath.Join(tenantProfileDir(cfg, repo), ".context")
	}
	summary, err := contextupdate.Run(ctx, repo, contextupdate.Config{
		ContextDir: contextDir,
		RepoRoot:   ".",
		Log:        logger,
	})
	if err != nil {
		return contextupdate.Summary{}, err
	}
	logger.Info("context update complete",
		"applications_updated", summary.ApplicationsUpdated,
		"career_updated", summary.CareerUpdated,
		"architecture_updated", summary.ArchitectureUpdated,
		"active_interviews", summary.ActiveInterviews,
		"rejections", summary.Rejections,
		"recent_prs", summary.RecentPRs,
		"context_dir", contextDir,
	)
	return summary, nil
}

func writeDescriptionHealth(path string, health db.DescriptionHealth) error {
	if path == "" {
		path = "jd-health.json"
	}
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(health, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o644)
}

func mustOpenRepo(logger *slog.Logger, cfg config.Config) *db.Repository {
	repo, err := openRepo(cfg)
	if err != nil {
		logger.Error("open database failed", "db_type", cfg.DBType, "error", err)
		os.Exit(1)
	}
	return repo
}

type rejectingVerifier struct{ err error }

func (v rejectingVerifier) Verify(context.Context, string) (string, error) {
	if v.err != nil {
		return "", v.err
	}
	return "", errors.New("auth verifier unavailable")
}

// buildDashboardAuth returns the dashboard's JWT verifier, or nil to run the
// dashboard unauthenticated. Hosted auth is enabled only on the Postgres backend
// with SUPABASE_URL configured. A JWKS setup failure logs and fails closed:
// protected routes still require auth, but no token can verify until the service
// restarts with a working Supabase JWKS connection.
func buildDashboardAuth(ctx context.Context, cfg config.Config, logger *slog.Logger) middleware.Verifier {
	if cfg.DBType != string(db.Postgres) || cfg.SupabaseURL == "" {
		return nil
	}
	v, err := auth.NewSupabaseVerifier(ctx, cfg.SupabaseURL)
	if err != nil {
		logger.Error("hosted auth unavailable: supabase jwks setup failed; protected routes will reject requests", "error", err)
		return rejectingVerifier{err: err}
	}
	logger.Info("hosted auth enabled", "supabase_url", cfg.SupabaseURL)
	return v
}

// openRepo opens the storage backend selected by DATABASE_TYPE: SQLite at
// cfg.DBPath (default, self-host) or Postgres at cfg.DatabaseURL (hosted SaaS).
func openRepo(cfg config.Config) (*db.Repository, error) {
	repo, err := openBaseRepo(cfg)
	if err != nil {
		return nil, err
	}
	// Hosted Postgres: the background pipeline (scrape/score/maintenance/discovery)
	// and the CLI one-shots run outside any HTTP request, so they cannot inherit a
	// tenant from the auth middleware the way the dashboard does. Left unscoped the
	// repo stays on LocalUser, and every scraped lead, event, and API tally lands
	// in an orphan 'local' partition the tenant's dashboard never sees. Scope it to
	// the active tenant here. The dashboard re-scopes per request via ForUser, so a
	// tenant-scoped base repo is correct for it too. Self-host SQLite resolves to
	// LocalUser, leaving that path byte-for-byte unchanged.
	if repo.DBType() == db.Postgres {
		if tenant, terr := repo.ActiveTenant(); terr == nil && tenant != "" && tenant != db.LocalUser {
			repo = repo.ForUser(tenant)
		}
	}
	return repo, nil
}

func pgPool(cfg config.Config) db.PoolConfig {
	return db.PoolConfig{
		MaxOpenConns:    cfg.DBMaxOpenConns,
		MaxIdleConns:    cfg.DBMaxIdleConns,
		ConnMaxLifetime: cfg.DBConnMaxLifetime,
		ConnMaxIdleTime: cfg.DBConnMaxIdleTime,
	}
}

// openBaseRepo opens the service-role repository the background crons, scorer, and
// migrator share. It always connects with full (BYPASSRLS) privileges so it can run
// DDL and the cross-tenant reads (Tenants/ActiveTenant) the per-tenant fan-out needs.
// RLS is layered on only at the dashboard request path; see dashboardRepo.
func openBaseRepo(cfg config.Config) (*db.Repository, error) {
	if cfg.DBType == string(db.Postgres) {
		return db.OpenPostgres(cfg.DatabaseURL, pgPool(cfg), cfg.Timezone, false)
	}
	return db.Open(cfg.DBPath)
}

// dashboardRepo returns the repository the dashboard serves user requests from.
// With RLS_ENFORCE on (Postgres only), it is a second connection on the restricted
// (non-BYPASSRLS) role so the dormant 00002_rls.sql policies enforce per request:
// an accidental unscoped query fails closed instead of leaking another tenant's
// rows. The base (service-role) repo stays in use for crons. The base repo has
// already run migrations, so this connection skips DDL. On any open failure it logs
// and falls back to base, leaving app-layer scoping as the sole (still live)
// control. With the flag off (default) it returns base unchanged.
func dashboardRepo(cfg config.Config, base *db.Repository, log *slog.Logger) *db.Repository {
	if !cfg.RLSEnforce || cfg.DBType != string(db.Postgres) {
		return base
	}
	dsn := cfg.RLSDatabaseURL
	if dsn == "" {
		dsn = cfg.DatabaseURL
	}
	repo, err := db.OpenPostgres(dsn, pgPool(cfg), cfg.Timezone, true)
	if err != nil {
		log.Error("RLS serving repo open failed; dashboard falling back to service-role repo", "error", err)
		return base
	}
	log.Info("RLS enforcement active for dashboard request path")
	return repo
}
