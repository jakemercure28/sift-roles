// Package dashboard is the Go dashboard. The strangler migration of the Node
// dashboard is complete: every route is served natively here, so there is no
// longer a reverse-proxy fallback to Node. It is folded into the go-backend
// process, which serves it alongside the scrape/score schedulers.
package dashboard

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"job-search-automation/internal/db"
	"job-search-automation/internal/middleware"
	"job-search-automation/internal/rejectionsync"
	"job-search-automation/internal/scorer"
)

// RejectionScorer is the Gemini capability the dashboard needs: rejection-
// likelihood reasoning after a pipeline transition, and arbitrary prompts for the
// in-app help assistant (scorer.Scorer satisfies it). May be nil to disable both.
type RejectionScorer interface {
	ScoreRejection(ctx context.Context, job scorer.Job) (string, error)
	Ask(ctx context.Context, prompt string, maxTokens int) (string, error)
}

// Server is the dashboard. It serves static assets, the health probe, and every
// page/JSON route natively.
type Server struct {
	publicDir        string
	dataDir          string
	scrapeTriggerURL string
	repo             *db.Repository
	rejection        RejectionScorer
	rejectionSyncRun func(context.Context) (rejectionsync.Summary, error)
	rateDelay        time.Duration
	dailyLimit       int
	// scrapeSchedule is the cron expression for background scrape runs, surfaced
	// on /api/scoring-progress as the next-scrape time so the browser can show
	// when fresh jobs will arrive. Empty means "unknown" (field is then omitted).
	scrapeSchedule string
	// hostKeyConfigured is true when a shared host Gemini key exists, so
	// /api/scoring-progress can tell the browser scoring is available. False on
	// self-host with no key configured.
	hostKeyConfigured bool
	log               *slog.Logger

	// authVerifier validates the request's bearer token in hosted mode. nil
	// means unauthenticated single-tenant self-host (every request is LocalUser).
	authVerifier middleware.Verifier
	// limiter throttles the expensive authenticated endpoints per tenant. nil
	// (the default / self-host) leaves every route unthrottled. Pointer-held so the
	// per-request forRequest clones share one set of token buckets.
	limiter *middleware.PerTenantLimiter
	// supabaseURL/supabaseAnonKey are handed to the browser (via /api/auth/config)
	// so supabase-js can run the OAuth flow. Empty on self-host.
	supabaseURL     string
	supabaseAnonKey string

	// market memoizes rendered Market Research bodies per tenant. Held by pointer
	// so the per-request Server clone (forRequest) shares one cache and stays
	// copy-safe (no mutex value is copied).
	market *marketBodyCache

	// jobs memoizes per-tenant filtered job sets behind a cheap jobs-table
	// signature so list-view interactions (filter/sort/page/search) between writes
	// don't re-pull the full row set from Postgres. The implementation is
	// pointer-held, so it is shared across forRequest clones; nil-safe (bypassed)
	// for bare test Servers. See jobsetcache.go.
	jobs jobSetCache

	// bodies memoizes rendered report bodies (analytics, activity-log) behind the
	// same per-tenant signature as jobs, so reopening a report between writes
	// skips its full-table scans and recompute. Pointer-held (shared across
	// forRequest clones); nil-safe (bypassed) for bare test Servers. See bodycache.go.
	bodies *bodyCache
}

// marketBodyCache memoizes rendered Market Research bodies. Building one loads
// every relevant job description and regex-classifies each one, so entries are
// keyed per tenant and only rebuild when the market-specific signature changes.
type marketBodyCache struct {
	mu      sync.Mutex
	entries map[string]bodyCacheEntry
}

func newMarketBodyCache() *marketBodyCache {
	return &marketBodyCache{entries: make(map[string]bodyCacheEntry)}
}

func (c *marketBodyCache) get(key, sig string, build func() (string, error)) (string, bool, error) {
	c.mu.Lock()
	ent, ok := c.entries[key]
	c.mu.Unlock()
	if ok && ent.sig == sig {
		return ent.html, true, nil
	}
	html, err := build()
	if err != nil {
		return "", false, err
	}
	c.mu.Lock()
	c.entries[key] = bodyCacheEntry{sig: sig, html: html}
	c.mu.Unlock()
	return html, false, nil
}

// SetScrapeTriggerURL configures the upstream the "Scrape now" button posts to.
// Empty (the default) makes /api/scrape-now report "not configured".
func (s *Server) SetScrapeTriggerURL(u string) { s.scrapeTriggerURL = u }

// SetHostKeyConfigured records whether a shared host Gemini key exists, surfaced
// on /api/scoring-progress so the browser knows scoring is available. False on
// self-host with no key configured.
func (s *Server) SetHostKeyConfigured(configured bool) {
	s.hostKeyConfigured = configured
}

// SetScrapeSchedule records the cron expression for background scrape runs so
// /api/scoring-progress can report the next scrape time. Empty (the default)
// omits the field.
func (s *Server) SetScrapeSchedule(cronExpr string) { s.scrapeSchedule = cronExpr }

// New builds the dashboard. repo backs the routes and the health probe;
// rejection may be nil; rateDelay and dailyLimit feed the scoring-progress ETA
// and quota derivation; dataDir locates location.json for the prefs route.
func New(publicDir string, repo *db.Repository, rejection RejectionScorer, rateDelay time.Duration, dailyLimit int, dataDir string, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		publicDir:  publicDir,
		dataDir:    dataDir,
		repo:       repo,
		rejection:  rejection,
		rateDelay:  rateDelay,
		dailyLimit: dailyLimit,
		log:        log,
		market:     newMarketBodyCache(),
		jobs:       newMemJobSetCache(defaultJobCacheMaxEntries, defaultJobCacheIdleTTL),
		bodies:     newBodyCache(),
	}, nil
}

// SetAuth enables hosted authentication: requests must carry a valid Supabase
// bearer token (verified by v), and each request's Repository is scoped to that
// token's user. url/anonKey are exposed to the browser so supabase-js can run the
// OAuth flow. Leaving this unset (self-host) keeps the service single-tenant
// (LocalUser) with no login UI.
func (s *Server) SetAuth(v middleware.Verifier, url, anonKey string) {
	s.authVerifier = v
	s.supabaseURL = url
	s.supabaseAnonKey = anonKey
}

// SetRateLimit enables per-tenant throttling of the expensive authenticated
// endpoints (market research, ask, scrape-now, profile refresh/extract). perMinute
// <= 0 (the default) disables it, leaving self-host and existing behavior unchanged.
func (s *Server) SetRateLimit(perMinute, burst int) {
	s.limiter = middleware.NewPerTenantLimiter(perMinute, burst)
}

// forUser returns a shallow copy of the Server whose Repository and profile dir
// are scoped to one tenant. It shares the root Server's pointer-held caches
// (rate limiters, bodies, market) so background callers and HTTP requests warm
// and read the same memo. forRequest layers request-scoped logging on top.
func (s *Server) forUser(uid string) *Server {
	clone := *s
	clone.repo = s.repo.ForUser(uid)
	if uid != "" {
		clone.log = clone.log.With("user_id", uid)
	}
	// Sandbox this tenant's profile files alongside its rows. On self-host
	// (SQLite/LocalUser) this is a no-op and dataDir stays the legacy root.
	clone.dataDir = tenantDataDir(s.dataDir, s.repo.DBType(), uid)
	if clone.dataDir != s.dataDir {
		// First touch from a fresh tenant: create and seed the profile dir with
		// structured-but-empty templates so the dashboard has files to read and the
		// setup wizard has somewhere to write. Idempotent for returning tenants.
		if err := provisionTenant(clone.dataDir); err != nil {
			clone.log.Warn("tenant provisioning failed", "error", err)
		}
	}
	return &clone
}

// forRequest returns a per-request shallow copy of the Server whose Repository is
// scoped to the tenant the auth middleware resolved (LocalUser on self-host). All
// downstream handler/helper calls then read and write only that tenant's rows.
func (s *Server) forRequest(r *http.Request) *Server {
	clone := s.forUser(middleware.UserID(r.Context()))
	// Stamp the request's trace onto this clone's logger so every handler's plain
	// s.log.Warn/Error call correlates to the request without per-call-site
	// changes. Derived from the root server's logger (scoped() always forRequests
	// from the root), so attributes never compound.
	if traceID := middleware.TraceID(r.Context()); traceID != "" {
		clone.log = clone.log.With("trace_id", traceID)
	}
	return clone
}

// WarmMarketResearch rebuilds and caches the Market Research body for one tenant
// so the next dashboard visit is a warm cache hit instead of the multi-second
// cold rebuild (which re-fetches every job over the remote pooler). It is cheap
// when nothing changed: renderMarketResearchBodyStatus runs only the signature
// query and returns the memoized body on a match. Safe to call from background
// crons in the dashboard process, which share this Server's market cache.
func (s *Server) WarmMarketResearch(uid string) {
	if s.market == nil {
		return
	}
	clone := s.forUser(uid)
	start := time.Now()
	_, status, err := clone.renderMarketResearchBodyStatus("")
	if err != nil {
		clone.log.Warn("market research warm failed", "error", err)
		return
	}
	// Only a rebuild (cache miss) is worth logging; idle signature checks are not.
	if status != "hit" {
		clone.log.Info("market research warmed", "status", status, "dur", time.Since(start))
	}
}

// scorer returns the effective LLM scorer for this request. Every tenant scores
// on the shared host key, so this is always s.rejection (which may itself be nil
// when no host key is configured).
func (s *Server) scorer() RejectionScorer {
	return s.rejection
}

// peopleDir holds applicant.md / voice.md, edited from the Career page. Self-host
// keeps the legacy repo-root .context/people; hosted mode nests it under the
// tenant's storage dir (dataDir is already tenant-scoped by forRequest) so each
// user's voice/applicant files stay isolated.
func (s *Server) peopleDir() string {
	if s.repo.DBType() == db.Postgres && s.repo.UserID() != db.LocalUser {
		return filepath.Join(s.dataDir, ".context", "people")
	}
	root, err := os.Getwd()
	if err != nil {
		root = "."
	}
	return filepath.Join(root, ".context", "people")
}

// scoped adapts a handler method expression into an http.HandlerFunc that runs it
// against the per-request tenant-scoped Server. Registering handlers as method
// expressions (e.g. (*Server).handleDashboardList) keeps the tenancy wiring in
// one place instead of repeating s = s.forRequest(r) in every handler.
func (s *Server) scoped(h func(*Server, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h(s.forRequest(r), w, r)
	}
}

// limited is scoped() plus per-tenant rate limiting, for the expensive endpoints
// (Gemini calls and scrape triggers). The limiter runs inside the request (after the
// Auth middleware has resolved the tenant), so it can key on UserID. With no limiter
// configured it is exactly scoped(), so unthrottled routes and self-host are
// unaffected.
func (s *Server) limited(h func(*Server, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return s.limiter.Limit(s.scoped(h)).ServeHTTP
}

// Handler returns the front-door HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public, unscoped routes: the page shell, static assets, health, and the
	// auth bootstrap config. These load before (or without) a session so the
	// browser can run the login flow, which then re-requests protected routes.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /public/", s.handleStatic)
	mux.HandleFunc("GET /api/auth/config", s.handleAuthConfig)

	// Tenant-scoped routes (ported from lib/dashboard-routes.js). Registered as
	// method expressions through scoped() so each runs against the per-request
	// tenant's Repository. More specific patterns than "/", so these win.
	mux.HandleFunc("GET /api/scraper-heartbeat", s.scoped((*Server).handleScraperHeartbeat))
	mux.HandleFunc("GET /api/scoring-progress", s.scoped((*Server).handleScoringProgress))
	mux.HandleFunc("GET /job-description", s.scoped((*Server).handleJobDescription))
	mux.HandleFunc("GET /company-notes", s.scoped((*Server).handleGetCompanyNotes))
	mux.HandleFunc("POST /company-notes", s.scoped((*Server).handleSaveCompanyNotes))
	mux.HandleFunc("POST /archive", s.scoped((*Server).handleArchive))
	mux.HandleFunc("POST /pipeline", s.scoped((*Server).handlePipeline))
	mux.HandleFunc("GET /api/location-prefs", s.scoped((*Server).handleGetLocationPrefs))
	mux.HandleFunc("POST /api/location-prefs", s.scoped((*Server).handlePostLocationPrefs))
	mux.HandleFunc("GET /api/dashboard-list", s.scoped((*Server).handleDashboardList))
	mux.HandleFunc("GET /api/analytics/audit", s.scoped((*Server).handleAnalyticsAudit))
	mux.HandleFunc("GET /api/market-activity", s.scoped((*Server).handleMarketActivity))
	mux.HandleFunc("GET /api/market-research/audit", s.scoped((*Server).handleMarketResearchAudit))
	mux.HandleFunc("GET /api/setup/status", s.scoped((*Server).handleSetupStatus))
	mux.HandleFunc("GET /api/settings/env", s.scoped((*Server).handleSettingsEnvGet))
	mux.HandleFunc("GET /api/setup/career", s.scoped((*Server).handleCareerGet))
	mux.HandleFunc("POST /api/setup/resume", s.scoped((*Server).handleSetupResume))
	mux.HandleFunc("POST /api/setup/profile", s.scoped((*Server).handleSetupProfile))
	mux.HandleFunc("POST /api/setup/companies", s.scoped((*Server).handleSetupCompanies))
	mux.HandleFunc("DELETE /api/account", s.scoped((*Server).handleDeleteAccount))
	// api-key/test-key remain registered (no UI: the Settings tab was removed) so
	// the wizard path and the hosted no-op safety test still hold.
	mux.HandleFunc("POST /api/setup/api-key", s.scoped((*Server).handleSetupAPIKey))
	mux.HandleFunc("POST /api/setup/test-key", s.scoped((*Server).handleSetupTestKey))
	mux.HandleFunc("POST /api/setup/extract-profile", s.limited((*Server).handleExtractProfile))
	mux.HandleFunc("POST /api/setup/run-refresh", s.limited((*Server).handleSetupRunRefresh))
	mux.HandleFunc("POST /api/settings/env", s.scoped((*Server).handleSettingsEnvPost))
	mux.HandleFunc("POST /api/setup/career", s.scoped((*Server).handleCareerSave))
	mux.HandleFunc("POST /api/setup/experience", s.scoped((*Server).handleExperienceSave))
	mux.HandleFunc("POST /api/setup/experience/delete", s.scoped((*Server).handleExperienceDelete))
	mux.HandleFunc("POST /api/setup/career/structure", s.limited((*Server).handleCareerStructure))
	mux.HandleFunc("GET /market-research", s.scoped((*Server).handleMarketResearchRedirect))
	mux.HandleFunc("POST /market-research", s.limited((*Server).handleMarketResearch))
	mux.HandleFunc("POST /api/scrape-now", s.limited((*Server).handleScrapeNow))
	mux.HandleFunc("POST /api/jobs", s.scoped((*Server).handleImportJob))
	mux.HandleFunc("GET /api/integrations/rejection-sync", s.scoped((*Server).handleRejectionSyncStatus))
	mux.HandleFunc("POST /api/integrations/rejection-sync/run", s.scoped((*Server).handleRejectionSyncRun))
	mux.HandleFunc("POST /api/ask", s.limited((*Server).handleAsk))
	mux.HandleFunc("GET /report-problem", s.scoped((*Server).handleReportProblem))
	if os.Getenv("APP_ENV") == "development" || os.Getenv("DEBUG") == "true" {
		mux.HandleFunc("POST /api/dev/reset-onboarding", s.scoped((*Server).handleDevResetOnboarding))
	}
	mux.HandleFunc("GET /{$}", s.scoped((*Server).handleDashboardPage)) // exact "/" only

	// Every route is native; unmatched paths 404 (no Node proxy fallback).
	// Auth resolves the request tenant (no-op LocalUser when unauthenticated);
	// SecurityHeaders stamps CSP/nosniff/anti-framing on every response (including
	// 401s and static assets); Trace wraps the outside so every request is traced.
	//
	// Metrics wraps the mux directly, INSIDE Auth: Auth replaces the request with a
	// context-scoped clone (r.WithContext) before routing, and ServeMux records the
	// matched pattern on whichever request it receives. Sitting outside Auth made
	// Metrics read the pre-clone request, whose Pattern is always empty, so every
	// authenticated route collapsed into "unmatched". Inside Auth it sees the routed
	// request and labels by real pattern. The cost is that requests Auth rejects
	// before routing (401s) are no longer timed, which is fine: they had no pattern
	// to label and are not a latency signal.
	return middleware.Trace(middleware.SecurityHeaders(middleware.Auth(middleware.Metrics(mux), s.authVerifier, isPublicPath)))
}

// isPublicPath reports whether a route is reachable without a resolved identity.
// These are the page shell, static assets, the health probe, and the auth
// bootstrap config — everything the browser needs to load and run the login flow
// before it holds a token. All other routes require a verified tenant.
func isPublicPath(r *http.Request) bool {
	p := r.URL.Path
	switch p {
	case "/", "/healthz", "/report-problem", "/api/auth/config":
		return true
	}
	return strings.HasPrefix(p, "/public/")
}

// handleAuthConfig returns the bootstrap config the browser needs to initialize
// supabase-js. authEnabled is false on self-host (no url/key), so the frontend
// skips loading the SDK and shows no login UI.
func (s *Server) handleAuthConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"authEnabled": s.authVerifier != nil,
		"url":         s.supabaseURL,
		"anonKey":     s.supabaseAnonKey,
	})
}

// Listen serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Listen(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler()}

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("dashboard front door listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := s.repo.Ping(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// mimeTypes mirrors the allow-list in dashboard.js: only these extensions are
// served, everything else 404s.
var mimeTypes = map[string]string{
	".css": "text/css",
	".js":  "application/javascript",
	".pdf": "application/pdf",
}

// handleStatic serves files under /public/ with the same caching contract as
// dashboard.js: a ?v= query makes the asset immutable for a year, otherwise
// no-cache. css/js are gzipped when the client accepts it.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/public/")
	// Reject path traversal before touching the filesystem.
	if rel == "" || strings.Contains(rel, "..") {
		http.NotFound(w, r)
		return
	}
	clean := filepath.Clean(rel)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		http.NotFound(w, r)
		return
	}

	ext := strings.ToLower(filepath.Ext(clean))
	mime, ok := mimeTypes[ext]
	if !ok {
		http.NotFound(w, r)
		return
	}

	full := filepath.Join(s.publicDir, clean)
	f, err := os.Open(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	cacheControl := "no-cache"
	if r.URL.Query().Has("v") {
		cacheControl = "public, max-age=31536000, immutable"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("Vary", "Accept-Encoding")

	gzippable := (ext == ".css" || ext == ".js") && clientAcceptsGzip(r)
	if gzippable {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		gz := gzip.NewWriter(w)
		defer gz.Close()
		if _, err := io.Copy(gz, f); err != nil {
			s.log.Warn("static gzip copy failed", "path", clean, "error", err)
		}
		return
	}

	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f); err != nil {
		s.log.Warn("static copy failed", "path", clean, "error", err)
	}
}

func clientAcceptsGzip(r *http.Request) bool {
	for _, enc := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if strings.TrimSpace(strings.SplitN(enc, ";", 2)[0]) == "gzip" {
			return true
		}
	}
	return false
}
