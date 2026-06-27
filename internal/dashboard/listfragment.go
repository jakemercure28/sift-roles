package dashboard

import (
	"net/http"
	"strconv"
	"time"

	"job-search-automation/internal/db"
	"job-search-automation/internal/metrics"

	"golang.org/x/sync/errgroup"
)

// This file wires the native GET /api/dashboard-list fragment for list and
// report views, porting buildDashboardView + renderDashboardFragment
// (lib/dashboard-routes.js, lib/dashboard-html.js).
//
// The list and the header counts are both scoped to the saved location prefs
// (see statsForPrefs / applyLocationPrefs); the slug/jd health banners remain
// deferred.

var dashboardFilterIDs = map[string]bool{
	"all": true, "not-applied": true, "applied": true, "interviewing": true,
	"offers": true, "rejected": true, "closed": true, "archived": true, "ghosted": true,
	"analytics": true, "activity-log": true, "market-research": true,
}

var allowedSorts = map[string]bool{
	"score": true, "date": true, "location-asc": true, "location-desc": true,
}

// fragmentResponse is the JSON shape of GET /api/dashboard-list (key order matches
// the Node handler: ok, url, titleHtml, filtersHtml, mainHtml).
type fragmentResponse struct {
	OK          bool   `json:"ok"`
	URL         string `json:"url"`
	TitleHTML   string `json:"titleHtml"`
	FiltersHTML string `json:"filtersHtml"`
	MainHTML    string `json:"mainHtml"`
}

// handleDashboardList serves the list-view and report-view fragments natively.
func (s *Server) handleDashboardList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filter := q.Get("filter")
	if !dashboardFilterIDs[filter] {
		filter = "not-applied"
	}

	rawSort := q.Get("sort")
	sortKey := "score"
	if allowedSorts[rawSort] {
		sortKey = rawSort
	}
	if filter == "rejected" && rawSort == "" {
		sortKey = "date"
	}

	opts := SearchOptions{
		Q:             q.Get("q"),
		MinScore:      atoiDefault(q.Get("minScore"), 1),
		Page:          atoiDefault(q.Get("page"), 1),
		AnalysisError: q.Get("analysisError"),
	}

	// A dropdown selection navigates through this fragment endpoint (not the full
	// page), so the location write-params must be persisted here too, otherwise the
	// pick never saves and the re-rendered dropdown reverts to the prior metro.
	if q.Has("setMetro") || q.Has("setUnlisted") || q.Has("setRemote") {
		if err := s.applyLocationWriteParams(q); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	frag, err := s.buildListFragment(filter, sortKey, opts)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, frag)
}

func (s *Server) buildListFragment(filter, sortKey string, opts SearchOptions) (fragmentResponse, error) {
	totalStart := time.Now()
	prefs := loadPrefs(s.dataDir)

	// This is the endpoint the browser blocks on to hydrate the page in hosted mode,
	// so its latency is what the user feels. The body, header stats and scraper
	// heartbeat are independent and each is its own Postgres round trip; run them
	// concurrently rather than chaining the RTTs. Each goroutine writes its own
	// variable, so no locking is needed; errgroup returns the first error.
	var (
		body       string
		pagination *Pagination
		bodyCache  string
		stats      db.Stats
		hbPtr      *db.Heartbeat
	)
	var g errgroup.Group
	g.Go(func() error {
		var err error
		body, pagination, bodyCache, err = s.dashboardBody(filter, sortKey, opts, prefs)
		return err
	})
	g.Go(func() error {
		start := time.Now()
		var err error
		stats, err = s.statsForPrefs(prefs)
		metrics.ObserveDashboardFragment(filter, "stats", "none", time.Since(start))
		return err
	})
	g.Go(func() error {
		start := time.Now()
		if hb, ok, _ := s.repo.ScraperHeartbeat(); ok {
			hbPtr = &hb
		}
		metrics.ObserveDashboardFragment(filter, "heartbeat", "none", time.Since(start))
		return nil
	})
	if err := g.Wait(); err != nil {
		metrics.ObserveDashboardFragment(filter, "total", "error", time.Since(totalStart))
		return fragmentResponse{}, err
	}
	metrics.ObserveDashboardFragment(filter, "total", bodyCache, time.Since(totalStart))

	titleHTML := renderDashboardTitle(filter, stats, hbPtr)
	filtersHTML := renderFilters(filter, sortKey, opts, prefs, metroOrdered)
	mainHTML := renderDashboardMainContent(body, hbPtr)

	page := 1
	if pagination != nil {
		page = pagination.Page
	}
	canonical := buildDashboardHref(filter, sortKey, SearchOptions{Q: opts.Q, MinScore: opts.MinScore, Page: page})

	return fragmentResponse{
		OK:          true,
		URL:         canonical,
		TitleHTML:   titleHTML,
		FiltersHTML: filtersHTML,
		MainHTML:    mainHTML,
	}, nil
}

// mapJobs converts the DB rows to the render struct.
func mapJobs(rows []db.ListedJob) []Job {
	out := make([]Job, len(rows))
	for i, j := range rows {
		out[i] = Job{
			ID: j.ID, Title: j.Title, Company: j.Company, URL: j.URL, Platform: j.Platform,
			Location: j.Location, PostedAt: j.PostedAt, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt,
			Description: j.Description, Score: j.Score, Reasoning: j.Reasoning,
			RejectionReason: j.RejectionReason, Status: j.Status, Stage: j.Stage,
			AppliedAt: j.AppliedAt, RejectedFromStage: j.RejectedFromStage, RejectedAt: j.RejectedAt,
		}
	}
	return out
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
