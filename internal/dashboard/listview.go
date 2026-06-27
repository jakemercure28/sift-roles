package dashboard

import (
	"sort"
	"strconv"
	"strings"

	"job-search-automation/internal/db"
)

// This file ports the dashboard list data layer (fetchFilteredJobs, paginateJobs,
// applyDashboardSearch, computeStatsFromJobs) from lib/dashboard-routes.js and
// lib/dashboard-search.js. Rendering is wired in a later step.

// jobsPageSize mirrors JOBS_PAGE_SIZE in lib/dashboard-routes.js.
const jobsPageSize = 25

// orderByClauses mirrors the ORDER_BY map (no user input enters these).
var orderByClauses = map[string]string{
	"date-applied":   "score IS NULL ASC, applied_at DESC, created_at DESC",
	"date-posted":    "score IS NULL ASC, posted_at DESC, created_at DESC",
	"date-rejected":  "score IS NULL ASC, COALESCE(rejected_at, updated_at) DESC",
	"score-applied":  "score IS NULL ASC, score DESC, applied_at DESC, created_at DESC",
	"score-posted":   "score IS NULL ASC, score DESC, posted_at DESC, created_at DESC",
	"score-rejected": "score IS NULL ASC, score DESC, COALESCE(rejected_at, updated_at) DESC",
}

// resolveOrder returns the SQL ORDER BY clause and the (sortMode, orderKey, locSort)
// needed for the in-Go re-sort, mirroring fetchFilteredJobs.
func resolveOrder(filter, sortKey string) (orderBy, sortMode, orderKey, locSort string) {
	locSort = ""
	if sortKey == "location-asc" {
		locSort = "asc"
	} else if sortKey == "location-desc" {
		locSort = "desc"
	}
	sortMode = "score"
	if sortKey == "date" {
		sortMode = "date"
	}
	dateKey := "posted"
	if filter == "rejected" {
		dateKey = "rejected"
	}
	scoreKey := dateKey
	if filter == "applied" {
		scoreKey = "applied"
	}
	orderKey = scoreKey
	if sortMode == "date" {
		orderKey = dateKey
	}
	orderBy = orderByClauses[sortMode+"-"+orderKey]
	return
}

// fetchFilteredJobs returns the filtered, ordered rows for a list view with the
// full free-text fields populated. Used by the search path, which scans those
// fields; the default list load uses fetchFilteredJobsLight. SQL does the base
// ordering; date-by-posted and location sorts are finished in Go because posted_at
// holds non-ISO text and location uses the displayed normalized label.
func fetchFilteredJobs(repo *db.Repository, filter, sortKey string) ([]db.ListedJob, error) {
	orderBy, sortMode, orderKey, locSort := resolveOrder(filter, sortKey)
	jobs, err := repo.FilteredJobs(filter, orderBy)
	if err != nil {
		return nil, err
	}
	sortFilteredJobs(jobs, sortMode, orderKey, locSort)
	return jobs, nil
}

// fetchFilteredJobsLightSearch is the text-search fetch: it pushes the substring
// match into SQL and returns lightweight rows (no description/reasoning), so the
// search path no longer drags every matching row's multi-KB free-text fields over
// the pooler. The caller applies location prefs, paginates, and hydrates the
// free-text fields for only the visible page. opts is normalized so Q/MinScore
// match applyDashboardSearch.
func fetchFilteredJobsLightSearch(repo *db.Repository, filter, sortKey string, opts SearchOptions) ([]db.ListedJob, error) {
	n := normalizeViewOptions(opts)
	orderBy, sortMode, orderKey, locSort := resolveOrder(filter, sortKey)
	jobs, err := repo.FilteredJobsLightSearch(filter, orderBy, n.Q, n.MinScore)
	if err != nil {
		return nil, err
	}
	sortFilteredJobs(jobs, sortMode, orderKey, locSort)
	return jobs, nil
}

// fetchFilteredJobsLight is fetchFilteredJobs without the large free-text columns
// (description/reasoning/rejection_reasoning left empty). The list view filters,
// sorts and paginates on the small columns and hydrates the text for only the
// visible page, so this is the cheap query cached per (tenant, filter, sort).
func fetchFilteredJobsLight(repo *db.Repository, filter, sortKey string) ([]db.ListedJob, error) {
	orderBy, sortMode, orderKey, locSort := resolveOrder(filter, sortKey)
	jobs, err := repo.FilteredJobsLight(filter, orderBy)
	if err != nil {
		return nil, err
	}
	sortFilteredJobs(jobs, sortMode, orderKey, locSort)
	return jobs, nil
}

// fetchFilteredJobsLightPage uses SQL LIMIT/OFFSET for the common score-sorted
// path when the list has no location prefs and no free-text search. This keeps
// the default dashboard load from fetching or sorting rows it will not render.
func fetchFilteredJobsLightPage(repo *db.Repository, filter, sortKey string, opts SearchOptions) ([]db.ListedJob, Pagination, error) {
	n := normalizeViewOptions(opts)
	orderBy, sortMode, orderKey, locSort := resolveOrder(filter, sortKey)

	if sortMode != "score" || locSort != "" {
		jobs, err := fetchFilteredJobsLight(repo, filter, sortKey)
		if err != nil {
			return nil, Pagination{}, err
		}
		sortFilteredJobs(jobs, sortMode, orderKey, locSort)
		paged, pagination := paginateJobs(applyDashboardSearch(jobs, n), n.Page)
		return paged, pagination, nil
	}

	totalItems, err := repo.FilteredJobsCount(filter, n.MinScore)
	if err != nil {
		return nil, Pagination{}, err
	}
	totalPages := (totalItems + jobsPageSize - 1) / jobsPageSize
	if totalPages < 1 {
		totalPages = 1
	}
	page := n.Page
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	offset := (page - 1) * jobsPageSize

	jobs, err := repo.FilteredJobsLightPage(filter, orderBy, n.MinScore, jobsPageSize, offset)
	if err != nil {
		return nil, Pagination{}, err
	}
	pagination := Pagination{
		Page:       page,
		PageSize:   jobsPageSize,
		TotalItems: totalItems,
		TotalPages: totalPages,
		StartItem:  0,
		EndItem:    0,
	}
	if totalItems > 0 {
		pagination.StartItem = offset + 1
		pagination.EndItem = offset + len(jobs)
	}
	return jobs, pagination, nil
}

// sortFilteredJobs finishes the ordering SQL cannot: date-by-posted (posted_at is
// non-ISO text) and location (uses the displayed normalized label). Operates only
// on small columns, so it is identical for the full and lightweight fetches.
func sortFilteredJobs(jobs []db.ListedJob, sortMode, orderKey, locSort string) {
	if sortMode == "date" && orderKey == "posted" {
		sort.SliceStable(jobs, func(i, j int) bool {
			if jobs[i].Score == nil || jobs[j].Score == nil {
				if jobs[i].Score == nil && jobs[j].Score == nil {
					return postedTimestamp(jobs[i].PostedAt) > postedTimestamp(jobs[j].PostedAt)
				}
				return jobs[i].Score != nil
			}
			return postedTimestamp(jobs[i].PostedAt) > postedTimestamp(jobs[j].PostedAt)
		})
	}

	if locSort != "" {
		dir := 1
		if locSort == "desc" {
			dir = -1
		}
		sort.SliceStable(jobs, func(i, j int) bool {
			la := strings.ToLower(normalizeLocation(jobs[i].Location))
			lb := strings.ToLower(normalizeLocation(jobs[j].Location))
			if la == "" && lb == "" {
				return false
			}
			if la == "" {
				return false // blanks last
			}
			if lb == "" {
				return true
			}
			cmp := strings.Compare(la, lb)
			if dir < 0 {
				cmp = -cmp
			}
			return cmp < 0
		})
	}
}

// searchableFields builds the lowercased haystack from SEARCHABLE_JOB_FIELDS in
// lib/dashboard-search.js (non-empty fields joined by newlines).
func searchableText(j db.ListedJob) string {
	fields := []string{
		j.Title, j.Company, j.Location, j.Description, j.Reasoning,
		j.RejectionReason, j.Status, j.Stage, j.ApplyComplexity, j.Platform,
	}
	var parts []string
	for _, f := range fields {
		if f != "" {
			parts = append(parts, f)
		}
	}
	return strings.ToLower(strings.Join(parts, "\n"))
}

// applyDashboardSearch filters by the min-score floor and a case-insensitive
// substring query, mirroring applyDashboardSearch.
func applyDashboardSearch(jobs []db.ListedJob, opts SearchOptions) []db.ListedJob {
	n := normalizeViewOptions(opts)
	q := strings.ToLower(n.Q)

	var out []db.ListedJob
	for _, j := range jobs {
		if j.Score != nil && *j.Score < n.MinScore {
			continue
		}
		if q != "" && !strings.Contains(searchableText(j), q) {
			continue
		}
		out = append(out, j)
	}
	return out
}

// paginateJobs slices the list and returns pagination metadata, mirroring
// paginateJobs (25 per page, clamped page).
func paginateJobs(jobs []db.ListedJob, requestedPage int) ([]db.ListedJob, Pagination) {
	totalItems := len(jobs)
	pagination := paginationForTotal(totalItems, requestedPage)
	start := (pagination.Page - 1) * pagination.PageSize
	end := start + pagination.PageSize
	if end > totalItems {
		end = totalItems
	}
	return jobs[start:end], pagination
}

func paginationForTotal(totalItems, requestedPage int) Pagination {
	totalPages := (totalItems + jobsPageSize - 1) / jobsPageSize
	if totalPages < 1 {
		totalPages = 1
	}
	page := requestedPage
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}

	startItem := 0
	endItem := 0
	if totalItems > 0 {
		startItem = (page-1)*jobsPageSize + 1
		endItem = startItem + jobsPageSize - 1
		if endItem > totalItems {
			endItem = totalItems
		}
	}
	return Pagination{
		Page:       page,
		PageSize:   jobsPageSize,
		TotalItems: totalItems,
		TotalPages: totalPages,
		StartItem:  startItem,
		EndItem:    endItem,
	}
}

// statsForPrefs returns the header counts scoped to the active location filter.
// With no metro/remote filter it uses the single-scan GlobalStats; otherwise it
// recomputes the buckets from the location-filtered job set so the title's
// total/pending/applied/interviewing reflect the selected metro.
func (s *Server) statsForPrefs(prefs LocationPrefs) (db.Stats, error) {
	if len(prefs.Metros) == 0 && !prefs.RemoteOnly {
		return s.repo.GlobalStats()
	}
	rows, err := s.statsRowsCached()
	if err != nil {
		return db.Stats{}, err
	}
	return computeStatsFromJobs(applyLocationPrefs(rows, prefs)), nil
}

// statsRowsCached returns StatsRows (location/title/status/stage for every job)
// from the shared job-set cache, keyed on the tenant's jobs-table signature. The
// metro-filtered header counts re-run on every list load that has a location
// filter active, and the underlying rows are identical regardless of which metro
// is selected, so without a cache each metro/tab click dragged all ~10k small rows
// across the pooler again. Signature gating invalidates it the instant a scrape,
// score or pipeline transition touches the tenant's rows. s.jobs is nil for bare
// test Servers, which bypass the cache.
func (s *Server) statsRowsCached() ([]db.ListedJob, error) {
	if s.jobs == nil {
		return s.repo.StatsRows()
	}
	count, maxUpdated, err := s.repo.MarketDataSignature()
	if err != nil {
		return nil, err
	}
	sig := strconv.Itoa(count) + "|" + maxUpdated
	key := s.repo.UserID() + "|__statsrows"
	rows, _, err := s.jobs.Get(key, sig, func() ([]db.ListedJob, error) {
		return s.repo.StatsRows()
	})
	return rows, err
}

// computeStatsFromJobs buckets an in-memory slice with the same predicates as
// GlobalStats, mirroring computeStatsFromJobs (used for the location-filtered path).
func computeStatsFromJobs(jobs []db.ListedJob) db.Stats {
	var s db.Stats
	activeStages := map[string]bool{"phone_screen": true, "interview": true, "onsite": true, "offer": true}
	in := func(v string, set ...string) bool {
		for _, x := range set {
			if v == x {
				return true
			}
		}
		return false
	}
	for _, j := range jobs {
		status, stage := j.Status, j.Stage
		if !in(status, "archived", "rejected", "closed", "ghosted") {
			s.Total++
		}
		if !in(status, "applied", "responded", "archived", "closed", "rejected", "ghosted") &&
			!in(stage, "closed", "rejected", "ghosted") {
			s.NotApplied++
		}
		if in(status, "applied", "responded") && stage != "closed" {
			s.Applied++
		}
		if activeStages[stage] {
			s.Interviewing++
		}
		switch stage {
		case "offer":
			s.Offers++
		case "rejected":
			s.Rejected++
		case "closed":
			s.Closed++
		case "ghosted":
			s.Ghosted++
		}
		if status == "archived" {
			s.Archived++
		}
	}
	return s
}
