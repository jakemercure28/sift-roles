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
//	server rescore              re-score already-scored jobs in place (see the case body)
//	server batch list           show open + recent Batch API scoring batches
//	server batch evict <name>   force-close a wedged open batch (jobs return to the pool)
//	server canonicalize-once    resolve alternate ATS rows into canonical primary rows
//	server maintain-once        run the DB-maintenance pass (dedup + auto-ghost), then exit
//	server seed-registry        hunt new ATS boards from Common Crawl into the global registry, then exit
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
//	server opsagent             run the self-healing infra agent (Alertmanager webhook + sweep)
//	server healthcheck          Docker liveness: DB openable + worker reachable
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	"job-search-automation/internal/jobfilter"
	"job-search-automation/internal/logging"
	"job-search-automation/internal/logprune"
	"job-search-automation/internal/metrics"
	"job-search-automation/internal/middleware"
	"job-search-automation/internal/opsagent"
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

	case "rescore":
		// Re-score already-scored jobs in place after a rubric or prompt change.
		// Scores are overwritten, never nulled: a pending row with a NULL score is
		// what ArchiveStaleUnscored sweeps, and it ages by posted_at/first_seen_at,
		// so the obvious "null it and run score-once" recovery silently re-buries
		// anything past the stale window.
		//
		// Usage:
		//   rescore --user <uid> --ids <id,id,...>   re-score an explicit set
		//   rescore --user <uid> --years-capped      re-score rows a years shortfall buried
		//   [--max-score N] [--limit N] [--dry-run]
		fs := flag.NewFlagSet("rescore", flag.ExitOnError)
		uid := fs.String("user", "", "tenant user id (required)")
		idList := fs.String("ids", "", "comma-separated job ids to re-score")
		yearsCapped := fs.Bool("years-capped", false, "re-score rows whose stated mismatch was a years shortfall")
		maxScore := fs.Int("max-score", cfg.AutoArchiveThreshold, "with --years-capped, only rows at or below this score")
		limit := fs.Int("limit", 0, "cap how many rows are re-scored (0 = no cap)")
		dryRun := fs.Bool("dry-run", false, "list the selected rows and exit without scoring")
		if err := fs.Parse(args); err != nil {
			os.Exit(2)
		}
		var ids []string
		for _, id := range strings.Split(*idList, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
		if *uid == "" || (len(ids) == 0 && !*yearsCapped) {
			fs.Usage()
			os.Exit(2)
		}

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		base := mustOpenRepo(logger, cfg)
		defer base.Close()
		repo := base.ForUser(*uid)

		jobs, err := repo.JobsToRescore(ids, *yearsCapped, *maxScore, *limit)
		if err != nil {
			logger.Error("rescore selection failed", "error", err)
			os.Exit(1)
		}
		if len(jobs) == 0 {
			logger.Info("rescore: nothing selected")
			return
		}
		if *dryRun {
			for _, j := range jobs {
				fmt.Printf("%s\t%s\t%s\n", j.ID, j.Company, j.Title)
			}
			fmt.Printf("\n%d job(s) selected. Re-run without --dry-run to score them.\n", len(jobs))
			return
		}

		res, err := pipeline.RescoreJobs(ctx, repo, buildScorer(cfg, repo, logger), jobs,
			scoreConfig(cfg, cfg.GeminiAPIKey != "", repo), logger)
		if err != nil {
			logger.Error("rescore failed", "error", err)
			os.Exit(1)
		}
		logger.Info("rescore complete", "selected", len(jobs),
			"scored_ok", res.ScoredOK, "scored_failed", res.ScoredFailed, "unarchived", res.Unarchived)

	case "maintain-once":
		// Run the DB-maintenance pass (dedup + auto-ghost) once and exit.
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		runMaintenance(ctx, repo, cfg, logger, nil)

	case "seed-registry":
		// Hunt new ATS board slugs from Common Crawl, verify the live ones, and
		// upsert them into the global company_registry, then exit. Same work the
		// monthly cron does; safe to run on demand. Zero Gemini cost.
		// Usage: seed-registry [numCrawls]   (default 1; a larger count backfills
		// that many recent monthly crawls to maximize the initial catch).
		crawls := cfg.RegistrySeedCrawls
		if len(args) > 0 {
			if n, err := strconv.Atoi(args[0]); err == nil && n > 0 {
				crawls = n
			}
		}
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if _, err := runRegistrySeed(ctx, repo, logger, crawls); err != nil {
			logger.Error("seed-registry failed", "error", err)
			os.Exit(1)
		}

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

	case "batch":
		// Admin view of the Batch API scoring ledger.
		//   batch list          open batches (any age) + everything from the last 7 days
		//   batch evict <name>  force-close a wedged open batch; members return to the
		//                       sync pool, and a best-effort Gemini-side cancel runs
		//                       when the overflow key is configured.
		sub := ""
		if len(args) > 0 {
			sub = args[0]
		}
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		switch sub {
		case "list":
			since := time.Now().UTC().AddDate(0, 0, -7).Format("2006-01-02 15:04:05")
			batches, err := repo.RecentScoreBatches(since)
			if err != nil {
				logger.Error("batch list failed", "error", err)
				os.Exit(1)
			}
			if len(batches) == 0 {
				fmt.Println("No open batches and none created in the last 7 days.")
				return
			}
			fmt.Printf("%-38s %-28s %-10s %5s %8s  %s\n", "USER", "BATCH", "STATE", "JOBS", "AGE", "ERROR")
			for _, b := range batches {
				age := "?"
				if created, err := time.Parse("2006-01-02 15:04:05", b.CreatedAt); err == nil {
					age = time.Since(created).Truncate(time.Minute).String()
				}
				fmt.Printf("%-38s %-28s %-10s %5d %8s  %s\n", b.UserID, b.BatchName, b.State, b.JobCount, age, b.Error)
			}
		case "evict":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "Usage: batch evict <batch-name>   (see `batch list` for names)")
				os.Exit(2)
			}
			name := args[1]
			b, found, err := repo.GetScoreBatchByName(name)
			if err != nil {
				logger.Error("batch lookup failed", "batch", name, "error", err)
				os.Exit(1)
			}
			if !found {
				fmt.Fprintf(os.Stderr, "batch evict: no batch named %q\n", name)
				os.Exit(1)
			}
			if b.State != db.ScoreBatchSubmitted {
				fmt.Fprintf(os.Stderr, "batch evict: %s is already terminal (%s), nothing to do\n", name, b.State)
				os.Exit(1)
			}
			// CloseScoreBatch is tenant-scoped; re-scope to the batch's owner.
			if err := repo.ForUser(b.UserID).CloseScoreBatch(name, db.ScoreBatchCancelled, "evicted via CLI"); err != nil {
				logger.Error("batch evict failed", "batch", name, "error", err)
				os.Exit(1)
			}
			fmt.Printf("Evicted %s (user %s, %d jobs returned to the scoring pool).\n", name, b.UserID, b.JobCount)
			if cfg.GeminiOverflowAPIKey != "" {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				bc := scorer.NewBatchClient(cfg.GeminiOverflowAPIKey, logger)
				if err := bc.CancelScoreBatch(ctx, name); err != nil {
					logger.Warn("gemini-side cancel failed (ledger row is closed regardless)", "batch", name, "error", err)
				} else {
					fmt.Println("Gemini-side cancel requested.")
				}
			}
		default:
			fmt.Fprintln(os.Stderr, "Usage: batch list | batch evict <batch-name>")
			os.Exit(2)
		}

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
		if _, err := runClosedCheck(ctx, repo, logger, nil); err != nil {
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
		srv.SetMarketAllTimeWindowDays(cfg.MarketAllTimeWindowDays)
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

	case "opsagent":
		// Self-healing infra agent. Runs as its own workload in the monitoring
		// namespace: it receives Alertmanager webhooks, investigates with
		// read-only cluster/Prometheus/Argo/GitHub tools, and may apply a small
		// allowlisted set of remediations. It shares this binary (and therefore
		// this image) rather than shipping its own, but it opens no database and
		// touches nothing in the pipeline.
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := runOpsAgent(ctx, logger); err != nil {
			logger.Error("opsagent error", "error", err)
			os.Exit(1)
		}

	default:
		// Only a BARE invocation runs the long-lived server. An unrecognized
		// subcommand used to land here and boot a second full app instance,
		// schedulers and all, inside whatever container it was exec'd into: the
		// port binds fail loudly but the cron schedulers still register, so a
		// typo against prod silently arms a duplicate set of scheduled writes.
		// Fail closed instead.
		if cmd != "" {
			fmt.Fprintf(os.Stderr, "unknown subcommand %q\nRun with no arguments to start the server; see the usage comment at the top of this file for the subcommand list.\n", cmd)
			os.Exit(2)
		}
		repo := mustOpenRepo(logger, cfg)
		defer repo.Close()
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		// Repair half-finished onboarding before the first cron fires, so a tenant
		// stranded by an abandoned wizard is serviced from this boot rather than
		// waiting for the next fan-out.
		repairStalledProfiles(repo, cfg.DataDir, logger)
		sched := scheduler.New(client, repo, cfg.DataDir, cfg.ScrapeSchedule, nil, logger)
		sched.SetGlobalJobSeedLimit(cfg.GlobalJobSeedLimit, cfg.GlobalJobSeedMaxAgeDays)
		if cfg.GlobalScrapeEnabled {
			// Global-first cron: scrape the cross-tenant board union once per
			// cycle and feed tenants by distribution instead of N board scrapes.
			sched.EnableGlobalScrape(scheduler.GlobalScrapeConfig{
				BoardBatch:           cfg.GlobalScrapeBoardBatch,
				DistributeLimit:      cfg.GlobalDistributeLimit,
				InitialMaxAgeDays:    cfg.GlobalJobSeedMaxAgeDays,
				DistributeGraceHours: cfg.GlobalDistributeGraceHours,
				CloseGraceHours:      cfg.GlobalCloseGraceHours,
				CloseVerify:          cfg.GlobalCloseVerify,
				WorkdayRegistryBatch: cfg.GlobalWorkdayRegistryBatch,
				DistributeReserve:    cfg.GlobalDistributeReserve,
			})
		}
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

		if cfg.ScoringEnabled {
			// Scrape→score coupling: each tenant's scrape chains straight into a
			// budgeted scoring pass so fresh listings are ranked the same cycle
			// instead of waiting out the gap to the next scoring cron tick.
			if cfg.ScoreAfterScrape {
				sched.SetScoringHook(func(ctx context.Context, r *db.Repository) error {
					tryScoreTenant(r.UserID(), logger, func() {
						if _, err := runScoringPass(ctx, cfg, r, logger); err != nil {
							logger.Error("post-scrape scoring failed", "user_id", r.UserID(), "error", err)
						}
					})
					return nil
				})
			}
			// Onboarding burst: score a signup's best seed matches while its first
			// scrape is still running. Gated to tenants with zero scored rows:
			// RunOnceForUser also serves the "Scrape now" button, and established
			// tenants must not re-trigger the burst's quota allowance on every press
			// (they are covered by the post-scrape pass above).
			if cfg.OnboardBurstLimit > 0 {
				sched.SetOnboardBurstHook(func(ctx context.Context, r *db.Repository, keywords []string) error {
					if n, err := r.CountScored(); err != nil || n > 0 {
						return err
					}
					tryScoreTenant(r.UserID(), logger, func() {
						if _, err := runOnboardingBurst(ctx, cfg, r, keywords, logger); err != nil {
							logger.Error("onboarding burst failed", "user_id", r.UserID(), "error", err)
						}
					})
					return nil
				})
			}
		}

		// On-demand trigger: the dashboard's "Scrape now" button posts here to kick
		// off a scrape. It shares the scheduler's in-flight guard, so manual and
		// cron runs can't overlap. Reachable only inside the compose network.
		trig := trigger.New(sched, cfg.ScrapeTimeout, cfg.GlobalScrapeTimeout, logger)
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
		dash.SetMarketAllTimeWindowDays(cfg.MarketAllTimeWindowDays)
		// Fresh config per run so Sync now picks up credentials saved in the
		// Settings modal without a restart.
		dash.SetRejectionSyncRunner(func(runCtx context.Context) (rejectionsync.Summary, error) {
			return runRejectionSync(runCtx, config.Load(), repo, logger)
		})
		// A tenant returning from an inactive stretch was keep-warm gated (see
		// inactiveKeepWarmCap); their first dashboard load kicks a full scoring
		// pass right away instead of waiting for the cron. Fresh config so the
		// pass honors settings saved since boot; tryScoreTenant dedupes overlap.
		if cfg.ScoringEnabled {
			dash.SetScoringKick(cfg.ScoringActiveWindowDays, func(uid string) {
				fresh := config.Load()
				tr := repo.ForUser(uid)
				tryScoreTenant(uid, logger, func() {
					if _, err := runScoringPass(ctx, fresh, tr, logger); err != nil {
						logger.Warn("return-kick scoring failed", "user_id", uid, "error", err)
					}
				})
			})
		}
		go func() {
			if err := dash.Listen(ctx, cfg.DashboardAddr); err != nil {
				logger.Error("dashboard server error", "error", err)
			}
		}()

		// Scheduled scoring runs only when explicitly enabled.
		if cfg.ScoringEnabled {
			startScoringCron(ctx, cfg, repo, logger)
		}

		// Gemini Batch API backlog drain: submit + poll on its own slow tick.
		startBatchScoreLoop(ctx, cfg, repo, logger)

		// DB maintenance (dedup + auto-ghost), ported from the Node worker.
		startMaintenanceCron(ctx, cfg, repo, logger)
		// Gmail rejection sweep + context update on their own faster cadence
		// (IMAP + regex, no Gemini), so a rejection email reaches the pipeline
		// stages within the half hour instead of the 6-hour maintenance tick.
		startRejectionSyncCron(ctx, cfg, repo, logger)
		startCanonicalizeCron(ctx, cfg, repo, logger)
		// Monthly Common Crawl registry seed: grow the global company pool with
		// employers no tenant has encountered yet (zero Gemini). Global, so it runs
		// once against the root repo, not per tenant.
		startRegistrySeedCron(ctx, cfg, repo, logger)
		// Market research warms once a day on its own schedule (see comment in
		// runMaintenance): refreshing it loads every job's description, so it must not
		// ride the 30-minute maintenance fan-out.
		startMarketResearchCron(ctx, cfg, repo, logger)

		if cfg.ScrapeOnStart {
			// The startup scrape is a full cycle (global board union or the
			// all-tenant fan-out), so it gets the full-cycle budget: the flat
			// per-tenant timeout truncated the 800-board global pass on prod.
			runCtx, cancel := context.WithTimeout(ctx, cfg.GlobalScrapeTimeout)
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
				tryScoreTenant(tr.UserID(), logger, func() {
					res, err := runScoringPass(ctx, cfg, tr, logger)
					if err != nil {
						logger.Error("startup scoring failed", "user_id", tr.UserID(), "error", err)
					} else {
						logger.Info("startup scoring complete", "user_id", tr.UserID(), "scored_ok", res.ScoredOK, "scored_failed", res.ScoredFailed)
					}
				})
			})
		}
		// Keep the Market Research body cache hot in the background (this same
		// process serves the dashboard), so visits skip the cold rebuild that
		// re-fetches every job. Launched after the optional startup scrape/scoring
		// so the first warm renders settled data.
		go warmMarketResearchLoop(ctx, dash, repo, cfg.DataDir, cfg.MarketWarmInterval, logger)
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

// runOpsAgent serves the self-healing agent: the Alertmanager webhook receiver,
// this workload's /metrics, and an in-process cron for the periodic sweep.
//
// It deliberately does not open the database or talk to the scraper. Its blast
// radius is the allowlist in internal/opsagent/gate.go, bounded again by the
// RBAC on its ServiceAccount.
func runOpsAgent(ctx context.Context, logger *slog.Logger) error {
	agentCfg, err := opsagent.LoadConfig()
	if err != nil {
		return err
	}
	agent, err := opsagent.New(agentCfg, logger)
	if err != nil {
		return err
	}
	srv := opsagent.NewServer(agent, agentCfg)

	c := cron.New()
	if err := srv.StartSweep(c); err != nil {
		return fmt.Errorf("register sweep schedule %q: %w", agentCfg.SweepSchedule, err)
	}
	c.Start()
	defer c.Stop()

	httpSrv := &http.Server{
		Addr:              agentCfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	logger.Info("opsagent started",
		"addr", agentCfg.ListenAddr,
		"model", agentCfg.Model,
		"fallback_model", agentCfg.FallbackModel,
		"daily_call_budget", agentCfg.DailyCallBudget,
		"dry_run", agentCfg.DryRun,
		"enabled_actions", agentCfg.EnabledActionNames(),
		"writable_namespaces", agentCfg.WritableNamespaceNames(),
		"sweep_schedule", agentCfg.SweepSchedule,
	)

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
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
	return client.WithHostKey(cfg.GeminiAPIKey != "").
		WithOverflowKey(cfg.GeminiOverflowAPIKey, cfg.GeminiOverflowRateDelay).
		WithThinkingLevel(cfg.ScoringThinkingLevel)
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

// scoreConfig builds the pipeline knobs for a tenant. hostKey flags a run on
// the shared host key so ScoreUnscored also clamps the run to the host key's
// global daily ceiling. The repo scopes the tenant whose saved location prefs
// feed the pre-score prefilter.
func scoreConfig(cfg config.Config, hostKey bool, repo *db.Repository) pipeline.ScoreConfig {
	return pipeline.ScoreConfig{
		DailyLimit:              cfg.GeminiDailyLimit,
		Concurrency:             cfg.ScoringConcurrency,
		ArchiveThreshold:        cfg.AutoArchiveThreshold,
		BatchSize:               cfg.ScoringBatchSize,
		Prefilter:               locationPrefilter(cfg, repo),
		UseExtracts:             cfg.ScoringUseExtracts,
		ExtractBatchSize:        cfg.ScoringExtractBatchSize,
		ResumeHash:              tenantResumeHash(cfg, repo),
		HostKey:                 hostKey,
		HostDailyLimit:          cfg.GeminiHostDailyLimit,
		HostPerTenantDailyLimit: cfg.GeminiHostPerTenantDailyLimit,
	}
}

// tenantResumeHash hashes the tenant's resume.md for score stamping; a missing
// or empty resume yields "" and stamping is skipped.
func tenantResumeHash(cfg config.Config, repo *db.Repository) string {
	data, err := os.ReadFile(filepath.Join(tenantProfileDir(cfg, repo), "resume.md"))
	if err != nil {
		return ""
	}
	return scorer.HashResume(string(data))
}

// locationPrefilter returns the tenant's pre-score location gate, or nil when
// the flag is off or the tenant has no saved prefs (default prefs pass every
// job, so nil skips the per-job work entirely).
func locationPrefilter(cfg config.Config, repo *db.Repository) pipeline.PrefilterFunc {
	if !cfg.PrefilterLocation {
		return nil
	}
	prefs := jobfilter.Load(tenantProfileDir(cfg, repo))
	if prefs.IsDefault() {
		return nil
	}
	return func(location, title string) (bool, string) {
		return jobfilter.Evaluate(location, title, prefs)
	}
}

// runScoringPass scores a tenant's backlog with the Flash batch path.
// Centralizing it keeps the three call sites (score-once, startup, cron) identical.
func runScoringPass(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger) (pipeline.ScoreResult, error) {
	hostKey := cfg.GeminiAPIKey != ""
	// Extract-once runs ahead of the tenant's scoring so fresh listings have
	// shared facts ready by the time they are scored. Failures never block
	// scoring: a job without an extract scores via the full-description path.
	if cfg.ExtractEnabled {
		ex := scorer.NewExtractor(buildGeminiClient(cfg, repo, logger))
		if _, err := pipeline.ExtractPending(ctx, repo, ex, pipeline.ExtractConfig{
			DailyLimit:  cfg.ExtractDailyLimit,
			Concurrency: cfg.ScoringConcurrency,
		}, logger); err != nil {
			logger.Warn("extract pass failed", "user_id", repo.UserID(), "error", err)
		}
	}
	sc := scoreConfig(cfg, hostKey, repo)
	if keepWarm := inactiveKeepWarmCap(cfg, repo, logger); keepWarm > 0 {
		sc.MaxJobs = keepWarm
		// The per-pass cap alone is not a budget: the pipeline runs many passes a
		// day, so keep-warm also needs the daily ceiling to stay cheap.
		sc.InactiveDailyLimit = cfg.GeminiHostInactiveDailyLimit
		logger.Info("tenant inactive; keep-warm scoring only",
			"user_id", repo.UserID(), "max_jobs", keepWarm, "window_days", cfg.ScoringActiveWindowDays,
			"daily_limit", cfg.GeminiHostInactiveDailyLimit)
	}
	return pipeline.ScoreUnscored(ctx, repo, buildScorer(cfg, repo, logger), sc, logger)
}

// inactiveKeepWarmCap returns the keep-warm top-N cap when this tenant has no
// recent dashboard activity, or 0 (no cap, full scoring) for active tenants.
// Any read error fails open: mis-gating an active tenant is worse than
// spending a pass on an inactive one. When the tenant returns, the dashboard's
// scoring kick (SetScoringKick) plus a fresh last-seen stamp restore full
// scoring immediately.
func inactiveKeepWarmCap(cfg config.Config, repo *db.Repository, logger *slog.Logger) int {
	if cfg.ScoringActiveWindowDays <= 0 || cfg.ScoringInactiveTopN <= 0 {
		return 0
	}
	lastSeen, err := repo.DashboardLastSeen()
	if err != nil {
		logger.Warn("last-seen read failed; treating tenant as active", "user_id", repo.UserID(), "error", err)
		return 0
	}
	hasScores := true
	if lastSeen == "" {
		n, err := repo.CountScoredJobs()
		if err != nil {
			logger.Warn("scored-count read failed; treating tenant as active", "user_id", repo.UserID(), "error", err)
			return 0
		}
		hasScores = n > 0
	}
	if tenantActive(lastSeen, hasScores, cfg.ScoringActiveWindowDays, time.Now()) {
		return 0
	}
	return cfg.ScoringInactiveTopN
}

// tenantActive reports whether a tenant qualifies for full scoring: dashboard
// seen within the window, or never seen but with nothing scored yet (a
// brand-new signup mid-onboarding must not be starved before their first
// dashboard load writes a stamp). Stamps are fixed-width UTC
// "2006-01-02 15:04:05", so a lexical compare is a time compare.
func tenantActive(lastSeen string, hasScores bool, windowDays int, now time.Time) bool {
	if lastSeen == "" {
		return !hasScores
	}
	cutoff := now.UTC().AddDate(0, 0, -windowDays).Format("2006-01-02 15:04:05")
	return lastSeen >= cutoff
}

// scoringInFlight serializes scoring per tenant across the cron, the startup
// pass, the post-scrape hook, and the onboarding burst. Overlap would not
// corrupt anything (score writes are idempotent updates), but two passes would
// pick the same unscored rows and spend duplicate Gemini calls. A skipped pass
// is fine: the scoring cron remains the sweeper.
var scoringInFlight sync.Map

// tryScoreTenant runs fn unless a scoring pass for uid is already in flight, and
// reports whether fn ran.
func tryScoreTenant(uid string, logger *slog.Logger, fn func()) bool {
	if _, loaded := scoringInFlight.LoadOrStore(uid, struct{}{}); loaded {
		logger.Info("scoring already in flight for tenant; skipping", "user_id", uid)
		return false
	}
	defer scoringInFlight.Delete(uid)
	fn()
	return true
}

// runOnboardingBurst scores a brand-new tenant's best seed matches immediately.
// The client may be forced onto the billed overflow key by
// ONBOARD_BURST_PREFER_OVERFLOW; otherwise it is free-key-first, and a 429-dry
// free key already fails over to the overflow key on the first probe call via
// the client's cooldown, so the burst never sleeps out a dead key's backoff.
func runOnboardingBurst(ctx context.Context, cfg config.Config, repo *db.Repository, keywords []string, logger *slog.Logger) (pipeline.ScoreResult, error) {
	client := buildGeminiClient(cfg, repo, logger).WithPreferOverflow(cfg.OnboardBurstPreferOverflow)
	sc := scorer.New(client, tenantProfileDir(cfg, repo)).
		WithRescoreThreshold(cfg.ScoringRescoreThreshold)
	return pipeline.ScoreOnboardingBurst(ctx, repo, sc, keywords,
		scoreConfig(cfg, cfg.GeminiAPIKey != "", repo),
		pipeline.BurstConfig{Limit: cfg.OnboardBurstLimit}, logger)
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
	repairStalledProfiles(repo, dataDir, logger)
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

// repairStalledProfiles completes any tenant profile abandoned between the
// wizard's resume upload and its final search-terms step, so the tenant joins
// this cycle's fan-out instead of sitting at zero jobs forever. It runs ahead of
// every background fan-out (and once at startup) because the stall is invisible
// otherwise: nothing errors, the signup just never receives a job.
func repairStalledProfiles(repo *db.Repository, dataDir string, logger *slog.Logger) {
	for _, r := range db.RepairStalledProfiles(dataDir, repo.DBType()) {
		logger.Warn("stalled onboarding repaired; derived search terms from resume",
			"user_id", r.UserID, "terms", strings.Join(r.Terms, ", "))
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
				// Same activity verdict the scoring paths use, so the backlog
				// panels split along the line paid batch scoring actually drains.
				metrics.SetJobsPending(tr.UserID(), n, inactiveKeepWarmCap(cfg, tr, logger) == 0)
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
				// Batch API spend is ledgered under its own model key (so the
				// sync-quota gauges above stay honest); publish it as its own
				// series so total-spend panels can add the two.
				if used, err := tr.HostDailyAPIUsage(scorer.BatchModel); err == nil {
					if tokens, err := tr.HostDailyAPITokens(scorer.BatchModel); err == nil {
						metrics.SetGeminiBatchUsage(used, tokens.Prompt, tokens.CachedPrompt, tokens.Candidates, tokens.Total)
					}
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
		// Batch-scoring loop state is cross-tenant; read once per tick. Cheap
		// counts over the small score_batches ledger.
		if open, err := repo.CountOpenScoreBatches(); err == nil {
			sinceUTC := time.Now().UTC().Format("2006-01-02") + " 00:00:00"
			if jobsToday, err := repo.SumBatchJobsSubmittedSince(sinceUTC); err == nil {
				oldestAge := 0.0
				if created, err := repo.OldestOpenScoreBatchCreatedAt(); err == nil && created != "" {
					if ts, err := time.Parse("2006-01-02 15:04:05", created); err == nil {
						oldestAge = time.Since(ts).Seconds()
					}
				}
				metrics.SetScoreBatchState(open, jobsToday, oldestAge)
			}
		}
		// global_jobs is a single cross-tenant table, so read it once per tick (not
		// per tenant). A COUNT/COUNT(DISTINCT) is light: it never reads descriptions,
		// so it stays clear of the egress-sensitive description reads.
		if total, companies, err := repo.CountGlobalJobsCache(); err == nil {
			metrics.SetGlobalJobsCache(total, companies)
		}
		// company_registry is likewise global: one grouped COUNT per tick makes the
		// board inventory (the stage upstream of global_jobs) visible in Grafana.
		if counts, err := repo.CountRegistryBoardsByPlatform(); err == nil {
			metrics.SetRegistryBoards(counts)
		}
		// Active-user gauges: distinct tenants who opened their dashboard within
		// each trailing window (metadata.dashboard_last_seen). This is the real
		// engagement signal cost decisions need, since stage-change events
		// undercount tenants who browse their ranked list without moving a job.
		now := time.Now().UTC()
		for _, w := range []struct {
			label string
			since time.Time
		}{
			{"1d", now.AddDate(0, 0, -1)},
			{"7d", now.AddDate(0, 0, -7)},
			{"30d", now.AddDate(0, 0, -30)},
		} {
			if n, err := repo.CountActiveUsers(w.since.Format("2006-01-02 15:04:05")); err == nil {
				metrics.SetActiveUsers(w.label, n)
			}
		}
	}
	tick()
	// Every tick fans out per tenant (CountUnscored + DailyAPIUsage) plus a
	// Tenants() scan, so this is a small but constant stream of Postgres round trips
	// over the remote pooler. These gauges only need to be roughly live for Grafana,
	// so the interval is generous (METRICS_REFRESH_INTERVAL_MS, default 15 min) to
	// cut the round-trip volume.
	interval := cfg.MetricsRefreshInterval
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	t := time.NewTicker(interval)
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

// defaultMarketWarmInterval is the fallback re-warm cadence when
// MARKET_WARM_INTERVAL_MS is unset. Idle ticks cost one cheap signature query per
// tenant; a full rebuild runs only after a tenant's jobs change. The page also
// rebuilds lazily on view, so the warm loop only saves the occasional cold visit,
// which is why this is generous (30 min) rather than per-minute.
const defaultMarketWarmInterval = 30 * time.Minute

// warmMarketResearchLoop keeps each tenant's Market Research body cache hot in
// the dashboard process so a visit lands on the in-memory memo instead of the
// multi-second cold rebuild that re-fetches every job over the remote pooler.
// dash shares one market cache across its forUser clones, so warming here is what
// the HTTP path later reads. The first tick warms immediately (covers the cold
// cache after a deploy/restart); thereafter it re-warms only tenants whose data
// changed. interval <= 0 falls back to defaultMarketWarmInterval. Returns when
// ctx is cancelled.
func warmMarketResearchLoop(ctx context.Context, dash *dashboard.Server, repo *db.Repository, dataDir string, interval time.Duration, logger *slog.Logger) {
	tick := func() {
		forEachTenant(repo, dataDir, logger, func(tr *db.Repository) {
			dash.WarmMarketResearch(tr.UserID())
		})
	}
	tick()
	if interval <= 0 {
		interval = defaultMarketWarmInterval
	}
	t := time.NewTicker(interval)
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
			tryScoreTenant(tr.UserID(), logger, func() {
				if _, err := runScoringPass(ctx, cfg, tr, logger); err != nil {
					logger.Error("scheduled scoring failed", "user_id", tr.UserID(), "error", err)
				}
			})
		})
	})
}

// startBatchScoreLoop drives the Gemini Batch API backlog drain: every tick it
// polls open batches (applying finished results) and fills free batch slots
// with ACTIVE tenants' unscored jobs. Gated on SCORING_BATCH_API_ENABLED plus
// the paid overflow key (the free tier has no batch quota). All batch state is
// in the DB, so restarts mid-flight lose nothing.
func startBatchScoreLoop(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger) {
	if !cfg.ScoringBatchAPIEnabled {
		return
	}
	if cfg.GeminiOverflowAPIKey == "" {
		logger.Warn("batch scoring enabled but GEMINI_API_KEY_OVERFLOW is empty; batch loop not started")
		return
	}
	interval := time.Duration(cfg.BatchAPIPollMinutes) * time.Minute
	if interval <= 0 {
		interval = 20 * time.Minute
	}
	var running atomic.Bool
	logger.Info("batch-score loop started", "interval", interval.String())
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !running.CompareAndSwap(false, true) {
					logger.Info("batch-score pass skipped: previous pass still running")
					continue
				}
				runBatchScorePass(ctx, repo, logger)
				running.Store(false)
			}
		}
	}()
}

// batchSubmitRotation staggers which tenant gets first claim on the free batch
// slots each pass, so a large-backlog tenant can't monopolize them.
var batchSubmitRotation atomic.Uint64

// batchSubmitCooldownUntil is the unix time before which batch SUBMISSION is
// paused after a quota-exhausted (429) submit; polling and applying results
// keep running. In-memory on purpose: a restart clears it, and quota that came
// back sooner (a top-up) just waits out the remainder.
var batchSubmitCooldownUntil atomic.Int64

func runBatchScorePass(ctx context.Context, repo *db.Repository, logger *slog.Logger) {
	// Reload config each pass (dashboard-saved settings, key rotation) like the
	// other crons.
	fresh := config.Load()
	if !fresh.ScoringBatchAPIEnabled || fresh.GeminiOverflowAPIKey == "" {
		return
	}
	api := scorer.NewBatchClient(fresh.GeminiOverflowAPIKey, logger).WithThinkingLevel(fresh.ScoringThinkingLevel)
	bcfg := pipeline.BatchScoreConfig{
		MaxOpenBatches:   fresh.BatchAPIMaxOpenBatches,
		DailyJobLimit:    fresh.BatchAPIDailyJobLimit,
		MaxJobsPerSubmit: fresh.BatchAPIMaxJobsPerSubmit,
		ArchiveThreshold: fresh.AutoArchiveThreshold,
	}

	if _, err := pipeline.PollScoreBatches(ctx, repo, api, func(uid string) pipeline.BatchTenant {
		return batchTenant(fresh, repo, uid, logger)
	}, bcfg, logger); err != nil {
		logger.Warn("batch poll pass failed", "error", err)
	}

	if until := batchSubmitCooldownUntil.Load(); until > time.Now().Unix() {
		logger.Info("batch submit skipped: quota cooldown active",
			"until", time.Unix(until, 0).UTC().Format(time.RFC3339))
		return
	}

	tenants, err := repo.BackgroundTenants(fresh.DataDir)
	if err != nil {
		logger.Warn("batch tenant enumeration failed", "error", err)
		return
	}
	var active []pipeline.BatchTenant
	for _, uid := range tenants {
		t := batchTenant(fresh, repo, uid, logger)
		// Paid batch spend follows the same activity gate as full sync scoring:
		// inactive tenants stay on the free keep-warm path only.
		if inactiveKeepWarmCap(fresh, t.Repo, logger) > 0 {
			continue
		}
		active = append(active, t)
	}
	if len(active) == 0 {
		return
	}
	rot := int(batchSubmitRotation.Add(1)) % len(active)
	rotated := append(append([]pipeline.BatchTenant{}, active[rot:]...), active[:rot]...)
	if _, err := pipeline.SubmitScoreBatches(ctx, repo, api, rotated, bcfg, logger); err != nil {
		if errors.Is(err, scorer.ErrQuotaExhausted) {
			cooldown := time.Duration(fresh.BatchAPIQuotaCooldownMinutes) * time.Minute
			batchSubmitCooldownUntil.Store(time.Now().Add(cooldown).Unix())
			metrics.IncScoreBatchQuotaTrip()
			logger.Warn("batch submit quota exhausted; cooling down submissions",
				"cooldown", cooldown.String(), "error", err)
			return
		}
		logger.Warn("batch submit pass failed", "error", err)
	}
}

// batchTenant assembles one tenant's batch-scoring context from the same
// pieces the sync path uses (scorer, prefilter, resume hash), so the two
// scoring paths cannot drift.
func batchTenant(cfg config.Config, repo *db.Repository, uid string, logger *slog.Logger) pipeline.BatchTenant {
	tr := repo
	if uid != db.LocalUser {
		tr = repo.ForUser(uid)
	}
	return pipeline.BatchTenant{
		UserID:     uid,
		Repo:       tr,
		Scorer:     buildScorer(cfg, tr, logger),
		Prefilter:  locationPrefilter(cfg, tr),
		ResumeHash: tenantResumeHash(cfg, tr),
	}
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
// ctx cancellation (shutdown) stops the long HTTP passes instead of leaving
// them running through a SIGTERM. probeCache shares closed-check verdicts and
// board snapshots across every tenant in one maintenance sweep, so a posting
// held by N tenants is probed once; nil (the one-shot CLI paths) gets a fresh
// per-run cache.
func runMaintenance(ctx context.Context, repo *db.Repository, cfg config.Config, logger *slog.Logger, probeCache *closedcheck.Cache) {
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
	if cfg.ScoringStaleUnscoredDays > 0 {
		if n, err := repo.ArchiveStaleUnscored(cfg.ScoringStaleUnscoredDays); err != nil {
			logger.Error("stale-unscored archive failed", "error", err)
		} else if n > 0 {
			logger.Info("stale-unscored archive complete", "archived", n, "days", cfg.ScoringStaleUnscoredDays)
		}
	}
	if err := runLogPrune(cfg, logger); err != nil {
		logger.Warn("log prune failed", "error", err)
	}
	if _, err := runDescriptionCheck(repo, cfg, logger); err != nil {
		logger.Warn("description check failed", "error", err)
	}
	if _, err := runClosedCheck(ctx, repo, logger, probeCache); err != nil {
		logger.Warn("closed check failed", "error", err)
	}
	// Market research is intentionally NOT refreshed here. It loads every live and
	// all-time job's description into memory, so refreshing it on the 30-minute
	// maintenance fan-out re-read the whole jobs table from Postgres up to 48x/day
	// per tenant and was the dominant source of Supabase egress. It now warms once
	// a day via startMarketResearchCron, and the dashboard builds it lazily on view
	// (23h cache), so the page stays fresh without the background re-reads.
	if _, err := runDiscovery(ctx, cfg, repo, logger); err != nil {
		logger.Warn("company discovery failed", "error", err)
	}
	if _, err := runRegistryURLHarvest(ctx, cfg, repo, logger); err != nil {
		logger.Warn("registry url harvest failed", "error", err)
	}
	if _, err := runSlugHealth(ctx, cfg, repo, logger); err != nil {
		logger.Warn("slug validation failed", "error", err)
	}
	// Rejection sync + context update moved to their own faster cron
	// (startRejectionSyncCron); see RejectionSyncSchedule in config.
}

// startMaintenanceCron schedules runMaintenance on cfg.MaintenanceSchedule with a
// single-flight guard, and stops the cron when ctx is cancelled.
func startMaintenanceCron(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger) {
	startCron(ctx, logger, "maintenance", cfg.MaintenanceSchedule, func() {
		// Reload config each pass so settings saved via the dashboard (Gmail
		// credentials, pause toggle, auto-ghost window) apply on the next
		// scheduled run instead of waiting for a container restart.
		fresh := config.Load()
		probeCache := closedcheck.NewCache()
		forEachTenant(repo, fresh.DataDir, logger, func(tr *db.Repository) {
			runMaintenance(ctx, tr, fresh, logger, probeCache)
		})
	})
}

// startRejectionSyncCron schedules the Gmail rejection-email sweep (and the
// context update that persists its stage changes) on cfg.RejectionSyncSchedule,
// fanning out per tenant. This used to ride the 6-hour maintenance pass, which
// left rejection emails unreflected in the pipeline for most of a day; the sweep
// is IMAP + regex with no Gemini calls, so a half-hour cadence costs nothing but
// the mailbox fetch. runRejectionSync's own single-flight guard keeps this cron
// and the dashboard's Sync now button from overlapping.
func startRejectionSyncCron(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger) {
	startCron(ctx, logger, "rejection-sync", cfg.RejectionSyncSchedule, func() {
		// Reload config each pass so Gmail credentials saved via the dashboard
		// apply on the next scheduled run instead of after a restart.
		fresh := config.Load()
		forEachTenant(repo, fresh.DataDir, logger, func(tr *db.Repository) {
			if _, err := runRejectionSync(ctx, fresh, tr, logger); err != nil {
				logger.Warn("rejection email sync failed", "user_id", tr.UserID(), "error", err)
			}
			if _, err := runContextUpdate(ctx, fresh, tr, logger); err != nil {
				logger.Warn("context update failed", "user_id", tr.UserID(), "error", err)
			}
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
		// Market research spends the same shared Gemini key as scoring but was not
		// gated by it. Once the key was exhausted every tenant still got a refresh
		// attempt, each burning the full 429 retry ladder, so the fan-out cost more
		// wall-clock than the scoring it was starving. Check the host ceiling once
		// up front rather than rediscovering the wall per tenant. The manual
		// market-research-once subcommand stays ungated: that one is a human asking.
		if fresh.GeminiAPIKey != "" {
			ok, hostUsed, err := pipeline.HostQuotaAvailable(repo, fresh.GeminiHostDailyLimit)
			switch {
			case err != nil:
				// Fail open: a usage-read blip should not silently stop the report.
				logger.Warn("host quota check failed; running market research anyway", "error", err)
			case !ok:
				logger.Info("host-key daily quota exhausted; skipping market research refresh",
					"host_used_today", hostUsed, "host_limit", fresh.GeminiHostDailyLimit)
				return
			}
		}
		forEachTenant(repo, fresh.DataDir, logger, func(tr *db.Repository) {
			if _, err := runMarketResearch(ctx, fresh, tr, logger, false); err != nil {
				logger.Warn("scheduled market research refresh failed", "user_id", tr.UserID(), "error", err)
			}
		})
	})
}

// runCanonicalize resolves pending alternate ATS rows once and applies the
// canonicalization/alias DB transaction for each row. The Gemini candidate
// fallback is only enabled when the paid overflow key is configured: it wins
// on ~3% of the rows the deterministic resolvers can't handle, which is not
// worth spending the free key's 500 requests/day scoring budget on.
func runCanonicalize(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger) (ats.CanonicalizeReport, error) {
	var gemini ats.GeminiClient
	if cfg.GeminiAPIKey != "" && cfg.GeminiOverflowAPIKey != "" {
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

func runClosedCheck(ctx context.Context, repo *db.Repository, logger *slog.Logger, cache *closedcheck.Cache) (closedcheck.Result, error) {
	res, err := closedcheck.Run(ctx, repo, closedcheck.Config{Cache: cache, Log: logger})
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
	srv.SetMarketAllTimeWindowDays(cfg.MarketAllTimeWindowDays)
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
		DataDir:         tenantProfileDir(cfg, repo),
		TTLHours:        cfg.DiscoveryTTLHours,
		CandidateCount:  cfg.DiscoveryCandidateCount,
		BootstrapPasses: cfg.DiscoveryBootstrapPasses,
		Gemini:          gemini,
		Log:             logger,
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
	// Registry harvest is LLM-free and runs every pass (even when the paid Gemini
	// discovery above is cache-skipped): it adopts globally-verified boards that
	// have roles matching this tenant's search terms, so a board one tenant found
	// reaches every other tenant it fits without another Gemini call. A tenant
	// still in bootstrap gets boosted limits so a fresh signup pulls a useful
	// slice of the shared registry on its first pass instead of trickling in
	// steady-state increments.
	if dcfg.Registry != nil {
		limits := discovery.HarvestLimits{MaxProbe: cfg.HarvestProbeLimit, MaxAdopt: cfg.HarvestAdoptLimit}
		if discovery.InBootstrap(dcfg.DataDir) {
			limits = discovery.HarvestLimits{MaxProbe: cfg.HarvestBootstrapProbe, MaxAdopt: cfg.HarvestBootstrapAdopt}
		}
		hr, herr := discovery.Harvest(ctx, dcfg, limits)
		if herr != nil {
			logger.Warn("registry harvest failed", "error", herr)
		} else if hr.Adopted > 0 {
			logger.Info("registry harvest adopted boards",
				"adopted", hr.Adopted, "probed", hr.Probed, "boards", hr.Names)
		}
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

// runRegistryURLHarvest mines the tenant's stored apply URLs for ATS boards and
// records the live ones in the shared company_registry, so a board the scraper
// already reached becomes directly scrapeable by every other tenant it matches.
// It costs no Gemini calls. Boards already trusted in the registry are skipped so
// each pass probes only genuinely new boards, and steady state does no work.
func runRegistryURLHarvest(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger) (int, error) {
	if !cfg.RegistryURLHarvest || repo == nil {
		return 0, nil
	}
	urls, err := repo.DistinctATSJobURLs()
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	var boards []discovery.VerifiedBoard
	for _, u := range urls {
		b, ok := discovery.ParseATSBoardURL(u)
		if !ok {
			continue
		}
		key := discovery.BoardKey(b)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		boards = append(boards, b)
	}
	if len(boards) == 0 {
		return 0, nil
	}
	reg := repo.CompanyRegistry()
	known, err := reg.Known(ctx)
	if err != nil {
		logger.Warn("registry url harvest: registry load failed; probing all", "error", err)
		known = discovery.Suggested{}
	}
	novel := novelBoards(boards, known)
	if len(novel) == 0 {
		return 0, nil
	}
	live := discovery.VerifyBoards(ctx, nil, novel, 0)
	if len(live) == 0 {
		return 0, nil
	}
	if err := reg.Upsert(ctx, live); err != nil {
		return 0, err
	}
	logger.Info("registry url harvest complete",
		"boards_in_urls", len(boards), "probed", len(novel), "added", len(live))
	return len(live), nil
}

// novelBoards drops boards already trusted in the registry snapshot so the
// harvest probes only new ones.
func novelBoards(boards []discovery.VerifiedBoard, known discovery.Suggested) []discovery.VerifiedBoard {
	have := map[string]bool{}
	for _, s := range known.Greenhouse {
		have["greenhouse:"+discovery.SlugKey(s)] = true
	}
	for _, s := range known.Ashby {
		have["ashby:"+discovery.SlugKey(s)] = true
	}
	for _, s := range known.Lever {
		have["lever:"+discovery.SlugKey(s)] = true
	}
	for _, e := range known.Workday {
		have["workday:"+discovery.WorkdayKey(e)] = true
	}
	var out []discovery.VerifiedBoard
	for _, b := range boards {
		if key := discovery.BoardKey(b); key != "" && !have[key] {
			out = append(out, b)
		}
	}
	return out
}

// registrySeedSource attributes Common Crawl-seeded registry rows (first_seen_by).
const registrySeedSource = "__commoncrawl__"

// runRegistrySeed hunts new ATS board slugs from the newest `crawls` Common Crawl
// indexes, verifies the live ones, and upserts them into the global
// company_registry. It grows the shared company pool with employers no tenant has
// encountered yet, at zero Gemini cost; the per-tenant harvest then distributes
// them by search-term relevance. Global (not per-tenant), so it runs once against
// the root repo. The monthly cron passes 1 (only the newest crawl is new each
// month); a larger count backfills history.
func runRegistrySeed(ctx context.Context, repo *db.Repository, logger *slog.Logger, crawls int) (int, error) {
	if repo == nil {
		return 0, nil
	}
	before, err := repo.CountRegistryBoards()
	if err != nil {
		logger.Warn("registry seed: count failed", "error", err)
	}
	report, err := discovery.HarvestCommonCrawl(ctx, discovery.CommonCrawlConfig{Crawls: crawls, Log: logger})
	if err != nil {
		return 0, err
	}
	if len(report.Live) == 0 {
		logger.Info("registry seed: no live boards found", "crawls", report.Crawls, "candidates", report.Candidates)
		return 0, nil
	}
	if _, err := repo.UpsertRegistryBoards(report.Live, registrySeedSource); err != nil {
		return 0, err
	}
	after, err := repo.CountRegistryBoards()
	if err != nil {
		logger.Warn("registry seed: count failed", "error", err)
		after = before
	}
	logger.Info("registry seed complete",
		"crawls", report.Crawls, "candidates", report.Candidates, "live", len(report.Live),
		"registry_before", before, "registry_after", after, "net_new", after-before)
	return after - before, nil
}

// startRegistrySeedCron schedules the Common Crawl registry seed on
// cfg.RegistrySeedSchedule. An empty schedule disables it. It sweeps
// cfg.RegistrySeedCrawls crawls per run (default 1: only the newest crawl is new
// each month).
func startRegistrySeedCron(ctx context.Context, cfg config.Config, repo *db.Repository, logger *slog.Logger) {
	if cfg.RegistrySeedSchedule == "" {
		logger.Info("registry seed cron disabled (empty schedule)")
		return
	}
	startCron(ctx, logger, "registry-seed", cfg.RegistrySeedSchedule, func() {
		if _, err := runRegistrySeed(ctx, repo, logger, cfg.RegistrySeedCrawls); err != nil {
			logger.Warn("scheduled registry seed failed", "error", err)
		}
	})
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
