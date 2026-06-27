package dashboard

import (
	"net/http"
	"net/url"

	"job-search-automation/internal/db"

	"golang.org/x/sync/errgroup"
)

// handleDashboardPage serves the full "/" page natively.
func (s *Server) handleDashboardPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	filter := q.Get("filter")
	if !dashboardFilterIDs[filter] {
		// Jobs is the pending-only triage inbox and the default landing view;
		// the full list lives under More > All Jobs (filter=all).
		filter = "not-applied"
	}
	if q.Has("setMetro") || q.Has("setUnlisted") || q.Has("setRemote") {
		if err := s.applyLocationWriteParams(q); err != nil {
			s.log.Error("location prefs save failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
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

	page, err := s.buildPage(filter, sortKey, opts)
	if err != nil {
		s.log.Error("dashboard page build failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write([]byte(page))
}

func (s *Server) buildPage(filter, sortKey string, opts SearchOptions) (string, error) {
	prefs := loadPrefs(s.dataDir)

	// In hosted/auth mode RenderPage paints the title, filters and main regions as
	// skeletons and the browser hydrates them via /api/dashboard-list once it has a
	// bearer token (see auth.js). Computing the real body/stats/heartbeat here is
	// therefore thrown away, so skip those queries entirely. This turns the first
	// paint into a near-static response instead of a full DB round trip.
	if s.authVerifier != nil {
		return RenderPage(PageView{
			Filter:          filter,
			Sort:            sortKey,
			Search:          opts,
			Prefs:           prefs,
			AuthEnabled:     true,
			SupabaseURL:     s.supabaseURL,
			SupabaseAnonKey: s.supabaseAnonKey,
		}), nil
	}

	// Self-host renders real data inline. The body, header stats, API-usage badge
	// and scraper heartbeat are independent and each costs a separate Postgres round
	// trip; serializing them stacks several RTTs onto every page load. Fetch them
	// concurrently and assemble after Wait. Each goroutine writes its own variable,
	// so no shared-state locking is needed. errgroup returns the first error.
	var (
		body    string
		stats   db.Stats
		hbPtr   *db.Heartbeat
	)
	var g errgroup.Group
	g.Go(func() error {
		var err error
		body, _, _, err = s.dashboardBody(filter, sortKey, opts, prefs)
		return err
	})
	g.Go(func() error {
		var err error
		stats, err = s.statsForPrefs(prefs)
		return err
	})
	g.Go(func() error {
		if hb, ok, _ := s.repo.ScraperHeartbeat(); ok {
			hbPtr = &hb
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		return "", err
	}

	return RenderPage(PageView{
		Filter:          filter,
		Sort:            sortKey,
		Search:          opts,
		Stats:           stats,
		Prefs:           prefs,
		Heartbeat:       hbPtr,
		BodyHTML:        body,
		AuthEnabled:     s.authVerifier != nil,
		SupabaseURL:     s.supabaseURL,
		SupabaseAnonKey: s.supabaseAnonKey,
	}), nil
}

func (s *Server) applyLocationWriteParams(q url.Values) error {
	current := loadPrefs(s.dataDir)
	includeUnknown := current.IncludeUnknown
	remoteOnly := current.RemoteOnly
	raw := rawPrefs{Metros: current.Metros, IncludeUnknown: &includeUnknown, RemoteOnly: &remoteOnly}
	if q.Has("setMetro") {
		metroKey := q.Get("setMetro")
		if metroKey == "" {
			raw.Metros = []string{}
		} else {
			raw.Metros = []string{metroKey}
		}
	}
	if q.Has("setUnlisted") {
		v := q.Get("setUnlisted") != "0"
		raw.IncludeUnknown = &v
	}
	if q.Has("setRemote") {
		v := q.Get("setRemote") == "1"
		raw.RemoteOnly = &v
	}
	_, err := savePrefs(s.dataDir, raw)
	return err
}
