package dashboard

import (
	"strconv"
	"strings"
	"time"

	"job-search-automation/internal/db"
	"job-search-automation/internal/metrics"

	"golang.org/x/sync/errgroup"
)

// cachedBody serves a rendered report body from s.bodies behind the per-tenant
// jobs-table signature, building it on a miss. Bare test Servers (s.bodies nil)
// bypass the cache and build directly.
func (s *Server) cachedBody(view string, build func() (string, error)) (string, bool, error) {
	if s.bodies == nil {
		html, err := build()
		return html, false, err
	}
	count, maxUpdated, err := s.repo.MarketDataSignature()
	if err != nil {
		return "", false, err
	}
	// Fold the day into the signature so time-relative metrics (stale-job windows,
	// the 4-week activity band) refresh at least daily even when no write moves the
	// jobs fingerprint; any scrape/score/pipeline transition refreshes it sooner.
	sig := strconv.Itoa(count) + "|" + maxUpdated + "|" + time.Now().Format("2006-01-02")
	key := s.repo.UserID() + "|" + view
	return s.bodies.get(key, sig, build)
}

// buildAnalyticsData gathers the inputs for renderAnalytics (fetchAnalyticsData).
func (s *Server) buildAnalyticsData(now int64) (AnalyticsData, error) {
	metrics, err := computeAnalyticsPageMetrics(s.repo, now)
	if err != nil {
		return AnalyticsData{}, err
	}
	rej, err := s.repo.RejectionInsights()
	if err != nil {
		return AnalyticsData{}, err
	}
	return AnalyticsData{Metrics: metrics, RejectionInsights: rej}, nil
}

// dashboardBody renders the dashboard body for a filter: the job table for list
// views, or the analytics / event-log / market-research reports. pagination is
// nil for non-list views.
func (s *Server) dashboardBody(filter, sortKey string, opts SearchOptions, prefs LocationPrefs) (string, *Pagination, string, error) {
	start := time.Now()
	switch filter {
	case "analytics":
		body, hit, err := s.cachedBody("analytics", func() (string, error) {
			data, err := s.buildAnalyticsData(time.Now().UnixMilli())
			if err != nil {
				return "", err
			}
			return renderAnalytics(data), nil
		})
		cache := cacheLabel(hit)
		metrics.ObserveDashboardFragment(filter, "body", cache, time.Since(start))
		return body, nil, cache, err
	case "activity-log":
		body, hit, err := s.cachedBody("activity-log", func() (string, error) {
			events, err := s.repo.RecentEvents()
			if err != nil {
				return "", err
			}
			return renderActivityLog(events), nil
		})
		cache := cacheLabel(hit)
		metrics.ObserveDashboardFragment(filter, "body", cache, time.Since(start))
		return body, nil, cache, err
	case "market-research":
		body, cache, err := s.renderMarketResearchBodyStatus(opts.AnalysisError)
		if err != nil {
			return "", nil, cache, err
		}
		metrics.ObserveDashboardFragment(filter, "body", cache, time.Since(start))
		return body, nil, cache, nil
	default:
		// A text query has to scan the large free-text fields (description/reasoning),
		// but it does so in SQL via fetchFilteredJobsLightSearch and still returns the
		// lightweight columns, so no path here drags every row's multi-KB text over the
		// pooler; the visible page is hydrated below. When the list has default location
		// prefs and score sort, the common path goes straight to SQL pagination so we
		// never fetch rows that will not render.
		nopts := normalizeViewOptions(opts)
		searching := strings.TrimSpace(nopts.Q) != ""
		sqlPaged := !searching && sortKey == "score" && len(prefs.Metros) == 0 && !prefs.RemoteOnly

		// The three reads are independent, so fetch them concurrently: against the
		// remote Postgres each is a separate round trip, and serializing them adds a
		// few RTTs to every list-view load. filteredJobsCached usually hits its cache,
		// but GetAppliedByCompany and CompanyTagsRaw always touch the DB.
		var (
			jobs             []db.ListedJob
			pagination       Pagination
			appliedByCompany map[string]int
			tagsRaw          map[string]string
			cacheHit         bool
		)
		var g errgroup.Group
		g.Go(func() error {
			var err error
			if sqlPaged {
				jobs, pagination, err = fetchFilteredJobsLightPage(s.repo, filter, sortKey, opts)
			} else if searching {
				jobs, err = fetchFilteredJobsLightSearch(s.repo, filter, sortKey, opts)
			} else {
				jobs, cacheHit, err = s.filteredJobsCached(filter, sortKey)
			}
			return err
		})
		g.Go(func() error {
			var err error
			appliedByCompany, err = s.repo.GetAppliedByCompany()
			return err
		})
		g.Go(func() error {
			var err error
			tagsRaw, err = s.repo.CompanyTagsRaw()
			return err
		})
		if err := g.Wait(); err != nil {
			metrics.ObserveDashboardFragment(filter, "body", "full_error", time.Since(start))
			return "", nil, "full_error", err
		}
		var paged []db.ListedJob
		switch {
		case sqlPaged:
			// fetchFilteredJobsLightPage already applied SQL LIMIT/OFFSET and set pagination.
			paged = jobs
		case searching:
			// fetchFilteredJobsLightSearch already applied the text match and min-score
			// floor in SQL, so the Go text filter is skipped (the light rows carry no
			// description/reasoning to re-scan). Only the metro/remote prefs remain.
			jobs = applyLocationPrefs(jobs, prefs)
			paged, pagination = paginateJobs(jobs, opts.Page)
		default:
			jobs = applyLocationPrefs(jobs, prefs)
			// No text query here, so applyDashboardSearch applies only the min-score floor.
			searched := applyDashboardSearch(jobs, opts)
			paged, pagination = paginateJobs(searched, opts.Page)
		}
		// Every fetch path now returns lightweight rows, so hydrate the description/
		// reasoning text for just the visible page (salary extraction, reasoning blurb).
		if err := s.hydrateJobText(paged); err != nil {
			metrics.ObserveDashboardFragment(filter, "body", "hydrate_error", time.Since(start))
			return "", nil, "hydrate_error", err
		}
		companyTags := make(map[string][]string, len(tagsRaw))
		for co, raw := range tagsRaw {
			companyTags[co] = parseCompanyTags(raw)
		}
		body := RenderJobTable(mapJobs(paged), appliedByCompany, companyTags, filter, sortKey, &pagination, opts)
		cache := "full_miss"
		if sqlPaged {
			cache = "paged"
		} else if searching {
			cache = "search"
		} else if cacheHit {
			cache = "full_hit"
		}
		metrics.ObserveDashboardFragment(filter, "body", cache, time.Since(start))
		return body, &pagination, cache, nil
	}
}

// hydrateJobText fills in the large free-text fields (description/reasoning/
// rejection reason) that the lightweight list fetch omits, for the given rows
// (the current page) in a single query. Mutates the passed slice in place; the
// rows are caller-owned copies (post-pagination), so the cache is untouched.
func (s *Server) hydrateJobText(jobs []db.ListedJob) error {
	if len(jobs) == 0 {
		return nil
	}
	ids := make([]string, len(jobs))
	for i := range jobs {
		ids[i] = jobs[i].ID
	}
	text, err := s.repo.JobTextByIDs(ids)
	if err != nil {
		return err
	}
	for i := range jobs {
		if t, ok := text[jobs[i].ID]; ok {
			jobs[i].Description = t.Description
			jobs[i].Reasoning = t.Reasoning
			jobs[i].RejectionReason = t.RejectionReason
		}
	}
	return nil
}

func cacheLabel(hit bool) string {
	if hit {
		return "hit"
	}
	return "miss"
}
