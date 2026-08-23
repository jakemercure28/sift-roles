package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"job-search-automation/internal/closedcheck"
	"job-search-automation/internal/db"
	"job-search-automation/internal/discovery"
	"job-search-automation/internal/model"
	"job-search-automation/internal/scraper"
)

// boardAwareWorker fakes the scraper service for global mode: board-mode
// requests return the given leads and per-board outcomes; profileDir-mode
// requests return nothing and record which platforms were asked for.
func boardAwareWorker(t *testing.T, boardCalls *[][]scraper.BoardRef, tenantPlatforms *[][]string, leads []map[string]string, boardResults []map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Platforms []string           `json:"platforms"`
			Boards    []scraper.BoardRef `json:"boards"`
		}
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		w.Header().Set("Content-Type", "application/json")
		if len(body.Boards) > 0 {
			*boardCalls = append(*boardCalls, body.Boards)
			resp := map[string]any{"count": len(leads), "platforms": []string{}, "jobs": leads}
			if len(boardResults) > 0 {
				resp["boards"] = boardResults
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		*tenantPlatforms = append(*tenantPlatforms, body.Platforms)
		_, _ = w.Write([]byte(`{"count":0,"platforms":[],"jobs":[]}`))
	}))
}

// TestRunOnceGlobalFirst drives one global-first cron cycle on self-host:
// the board union is scraped once in board mode, every lead is harvested,
// term-matching distribution feeds the tenant queue, the per-tenant scrape
// narrows to the aggregator platforms, and the cursor persists.
func TestRunOnceGlobalFirst(t *testing.T) {
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	dataDir := t.TempDir()
	companies := `{"SEARCH_TERMS":["devops"],"GREENHOUSE_COMPANIES":["acme"],"WORKDAY_COMPANIES":[{"sub":"megacorp","wd":5,"board":"External","label":"MegaCorp"}]}`
	if err := os.WriteFile(filepath.Join(dataDir, "companies.json"), []byte(companies), 0o644); err != nil {
		t.Fatalf("write companies.json: %v", err)
	}

	leads := []map[string]string{
		{
			"id": "greenhouse-1", "title": "DevOps Engineer", "company": "acme",
			"description": "infra work", "direct_apply_url": "https://boards.greenhouse.io/acme/jobs/1",
			"ats_platform_name": "Greenhouse", "scraped_timestamp": "2026-07-01T00:00:00.000Z",
			"location": "Remote", "posted_at": "",
		},
		{
			"id": "greenhouse-2", "title": "Underwater Basket Weaver", "company": "acme",
			"description": "weaving", "direct_apply_url": "https://boards.greenhouse.io/acme/jobs/2",
			"ats_platform_name": "Greenhouse", "scraped_timestamp": "2026-07-01T00:00:00.000Z",
			"location": "Remote", "posted_at": "",
		},
	}

	var boardCalls [][]scraper.BoardRef
	var tenantPlatforms [][]string
	srv := boardAwareWorker(t, &boardCalls, &tenantPlatforms, leads, nil)
	defer srv.Close()

	s := New(scraper.New(srv.URL, 5*time.Second), repo, dataDir, "@daily", nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.EnableGlobalScrape(GlobalScrapeConfig{BoardBatch: 10, DistributeLimit: 100})

	res, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// One board-mode call carrying the union (greenhouse board + workday board).
	if len(boardCalls) != 1 {
		t.Fatalf("board calls = %d, want 1", len(boardCalls))
	}
	byPlatform := map[string]scraper.BoardRef{}
	for _, b := range boardCalls[0] {
		byPlatform[b.Platform] = b
	}
	if byPlatform["greenhouse"].Slug != "acme" {
		t.Errorf("greenhouse board missing from union: %+v", boardCalls[0])
	}
	if wd := byPlatform["workday"]; wd.Slug != "megacorp" || wd.WD != 5 || wd.Board != "External" || wd.Label != "MegaCorp" {
		t.Errorf("workday board wrong in union: %+v", wd)
	}

	// The per-tenant scrape narrowed to aggregators only.
	if len(tenantPlatforms) != 1 {
		t.Fatalf("tenant scrapes = %d, want 1", len(tenantPlatforms))
	}
	for _, p := range tenantPlatforms[0] {
		if boardModePlatforms[p] {
			t.Errorf("tenant cron scrape still covers board platform %q", p)
		}
	}
	if len(tenantPlatforms[0]) == 0 {
		t.Error("tenant cron scrape got no platforms; aggregators must remain")
	}

	// Distribution kept the term match, dropped the non-match.
	unscored, err := repo.GetUnscoredJobs(10)
	if err != nil {
		t.Fatalf("GetUnscoredJobs: %v", err)
	}
	if len(unscored) != 1 || unscored[0].ID != "greenhouse-1" {
		t.Errorf("distributed queue = %+v, want exactly greenhouse-1", unscored)
	}
	if res.Inserted != 1 {
		t.Errorf("Inserted = %d, want 1 (distribution)", res.Inserted)
	}

	// Cursor persisted for the next incremental pass.
	if readDistributionCursor(dataDir) == "" {
		t.Error("distribution cursor not written")
	}

	// A second cycle distributes nothing new (idempotent + cursor).
	res, err = s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if res.Inserted != 0 {
		t.Errorf("second cycle Inserted = %d, want 0", res.Inserted)
	}
}

// TestDistributeRecoversStrandedBelowCursor reproduces the Gem stranding: a
// global row scraped early in a pass carries a lower last_seen_at than the row
// the tenant's cursor jumped to later in the same pass, so the strict > cursor
// forward query never revisits it. The grace-window backfill must recover it;
// with the grace disabled it stays stranded.
func TestDistributeRecoversStrandedBelowCursor(t *testing.T) {
	run := func(t *testing.T, graceHours int) *db.Repository {
		repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { repo.Close() })

		dataDir := t.TempDir()
		// Terms only, no companies and no boards: the union is empty, so the
		// board pass touches nothing and the pre-seeded global row keeps its
		// stale last_seen_at.
		companies := `{"SEARCH_TERMS":["platform engineer"]}`
		if err := os.WriteFile(filepath.Join(dataDir, "companies.json"), []byte(companies), 0o644); err != nil {
			t.Fatalf("write companies.json: %v", err)
		}

		// A matching Gem row; its last_seen_at is UTC now (datetime('now')).
		if err := repo.HarvestGlobalLead(seedLead("gem-strand", "Platform Engineer", "Nominal", "Gem")); err != nil {
			t.Fatalf("harvest: %v", err)
		}
		// Put the cursor two minutes ahead, as if a fresher lead later in the same
		// pass had advanced it past this row: the forward > cursor query now skips
		// the row, but it still sits inside the grace window below the cursor.
		cursor := time.Now().UTC().Add(2 * time.Minute).Format("2006-01-02 15:04:05")
		if err := writeDistributionCursor(dataDir, cursor); err != nil {
			t.Fatalf("write cursor: %v", err)
		}

		var boardCalls [][]scraper.BoardRef
		var tenantPlatforms [][]string
		srv := boardAwareWorker(t, &boardCalls, &tenantPlatforms, nil, nil)
		t.Cleanup(srv.Close)
		s := New(scraper.New(srv.URL, 5*time.Second), repo, dataDir, "@daily", nil,
			slog.New(slog.NewTextHandler(io.Discard, nil)))
		s.EnableGlobalScrape(GlobalScrapeConfig{BoardBatch: 10, DistributeLimit: 100, DistributeGraceHours: graceHours})
		if _, err := s.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		return repo
	}

	t.Run("grace recovers", func(t *testing.T) {
		if ids := unscoredIDs(t, run(t, 6)); !ids["gem-strand"] {
			t.Errorf("stranded Gem row not recovered by backfill: have %v", ids)
		}
	})
	t.Run("no grace strands", func(t *testing.T) {
		if ids := unscoredIDs(t, run(t, 0)); ids["gem-strand"] {
			t.Error("row distributed with grace disabled; the forward query should skip it")
		}
	})
}

// seedLead builds one already-held tenant row for diff-close tests.
func seedLead(id, title, company, platform string) model.Lead {
	return model.Lead{
		JobLead: model.JobLead{
			Title:            title,
			Company:          company,
			Description:      "desc for " + id,
			DirectApplyURL:   "https://example.com/" + id,
			ATSPlatformName:  platform,
			ScrapedTimestamp: "2026-07-01T00:00:00.000Z",
			Location:         "Remote",
		},
		ID: id,
	}
}

// unscoredIDs returns the set of pending unscored job ids, the visibility this
// test uses to tell open rows from diff-closed ones.
func unscoredIDs(t *testing.T, repo *db.Repository) map[string]bool {
	t.Helper()
	unscored, err := repo.GetUnscoredJobs(100)
	if err != nil {
		t.Fatalf("GetUnscoredJobs: %v", err)
	}
	ids := map[string]bool{}
	for _, u := range unscored {
		ids[u.ID] = true
	}
	return ids
}

// TestGlobalDiffSkipsIncompleteBoards reproduces the Workday mass-close
// incident: a board whose fetch came back partial (worker reports status
// "failed") still contributes leads, but its held rows must NOT be closed by
// the diff. A completely fetched board on the same cycle still closes its
// genuinely vanished rows.
func TestGlobalDiffSkipsIncompleteBoards(t *testing.T) {
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	dataDir := t.TempDir()
	companies := `{"SEARCH_TERMS":["engineer"],"GREENHOUSE_COMPANIES":["acme"],"WORKDAY_COMPANIES":[{"sub":"megacorp","wd":5,"board":"External","label":"MegaCorp"}]}`
	if err := os.WriteFile(filepath.Join(dataDir, "companies.json"), []byte(companies), 0o644); err != nil {
		t.Fatalf("write companies.json: %v", err)
	}

	for _, l := range []model.Lead{
		seedLead("wd-keep", "Platform Engineer", "MegaCorp", "Workday"),
		seedLead("wd-gone", "Systems Engineer", "MegaCorp", "Workday"),
		seedLead("gh-keep", "DevOps Engineer", "acme", "Greenhouse"),
		seedLead("gh-gone", "SRE Engineer", "acme", "Greenhouse"),
	} {
		if _, err := repo.InsertScrapedLead(l); err != nil {
			t.Fatalf("insert %s: %v", l.ID, err)
		}
	}
	// global_jobs.last_seen_at has second resolution; the cycle cutoff must
	// land strictly after the seed harvest.
	time.Sleep(1100 * time.Millisecond)

	// This cycle the worker re-saw wd-keep and gh-keep only. The Workday board
	// reports an incomplete fetch, the Greenhouse board reports nothing (its
	// completeness is implied by single-request fetching).
	leads := []map[string]string{
		{
			"id": "wd-keep", "title": "Platform Engineer", "company": "MegaCorp",
			"description": "desc", "direct_apply_url": "https://example.com/wd-keep",
			"ats_platform_name": "Workday", "scraped_timestamp": "2026-07-01T00:00:00.000Z",
			"location": "Remote", "posted_at": "",
		},
		{
			"id": "gh-keep", "title": "DevOps Engineer", "company": "acme",
			"description": "desc", "direct_apply_url": "https://example.com/gh-keep",
			"ats_platform_name": "Greenhouse", "scraped_timestamp": "2026-07-01T00:00:00.000Z",
			"location": "Remote", "posted_at": "",
		},
	}
	boardResults := []map[string]string{
		{"platform": "workday", "slug": "megacorp", "company": "MegaCorp", "status": "failed"},
	}

	var boardCalls [][]scraper.BoardRef
	var tenantPlatforms [][]string
	srv := boardAwareWorker(t, &boardCalls, &tenantPlatforms, leads, boardResults)
	defer srv.Close()

	s := New(scraper.New(srv.URL, 5*time.Second), repo, dataDir, "@daily", nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.EnableGlobalScrape(GlobalScrapeConfig{BoardBatch: 10, DistributeLimit: 100})

	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	open := unscoredIDs(t, repo)
	if !open["wd-gone"] {
		t.Error("wd-gone was closed by the diff despite the incomplete Workday fetch")
	}
	if open["gh-gone"] {
		t.Error("gh-gone survived the diff despite vanishing from a complete board")
	}
	if !open["wd-keep"] || !open["gh-keep"] {
		t.Errorf("re-seen rows were closed: %v", open)
	}
}

// TestGlobalDiffMassCloseSuppressed drives the circuit breaker: when one
// company's vanished set exceeds maxDiffClosesPerCompany in a single cycle,
// none of its rows are closed.
func TestGlobalDiffMassCloseSuppressed(t *testing.T) {
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	dataDir := t.TempDir()
	companies := `{"SEARCH_TERMS":["engineer"],"GREENHOUSE_COMPANIES":["acme"]}`
	if err := os.WriteFile(filepath.Join(dataDir, "companies.json"), []byte(companies), 0o644); err != nil {
		t.Fatalf("write companies.json: %v", err)
	}

	total := maxDiffClosesPerCompany + 2 // one re-seen, the rest vanished (> cap)
	for i := 0; i < total; i++ {
		l := seedLead(fmt.Sprintf("gh-%d", i), fmt.Sprintf("Engineer %d", i), "acme", "Greenhouse")
		if _, err := repo.InsertScrapedLead(l); err != nil {
			t.Fatalf("insert %s: %v", l.ID, err)
		}
	}
	time.Sleep(1100 * time.Millisecond)

	leads := []map[string]string{
		{
			"id": "gh-0", "title": "Engineer 0", "company": "acme",
			"description": "desc", "direct_apply_url": "https://example.com/gh-0",
			"ats_platform_name": "Greenhouse", "scraped_timestamp": "2026-07-01T00:00:00.000Z",
			"location": "Remote", "posted_at": "",
		},
	}

	var boardCalls [][]scraper.BoardRef
	var tenantPlatforms [][]string
	srv := boardAwareWorker(t, &boardCalls, &tenantPlatforms, leads, nil)
	defer srv.Close()

	s := New(scraper.New(srv.URL, 5*time.Second), repo, dataDir, "@daily", nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.EnableGlobalScrape(GlobalScrapeConfig{BoardBatch: 10, DistributeLimit: 100})

	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	open := unscoredIDs(t, repo)
	if len(open) != total {
		t.Errorf("open rows = %d, want all %d (mass close must be suppressed)", len(open), total)
	}
}

// TestGlobalDiffGraceWindow: with the close grace window on, a posting
// missing from a single complete fetch is NOT closed; only postings missing
// for the whole window are. Workday boards flap live postings out of one
// listing and back into a later one (nike/usc, 2026-07-05), so one missed
// cycle proves nothing.
func TestGlobalDiffGraceWindow(t *testing.T) {
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	dataDir := t.TempDir()
	companies := `{"SEARCH_TERMS":["engineer"],"GREENHOUSE_COMPANIES":["acme"]}`
	if err := os.WriteFile(filepath.Join(dataDir, "companies.json"), []byte(companies), 0o644); err != nil {
		t.Fatalf("write companies.json: %v", err)
	}

	for _, l := range []model.Lead{
		seedLead("gh-keep", "DevOps Engineer", "acme", "Greenhouse"),
		seedLead("gh-flap", "SRE Engineer", "acme", "Greenhouse"),
	} {
		if _, err := repo.InsertScrapedLead(l); err != nil {
			t.Fatalf("insert %s: %v", l.ID, err)
		}
	}
	time.Sleep(1100 * time.Millisecond)

	// This cycle the board only listed gh-keep. gh-flap vanished seconds ago,
	// well inside the grace window, so it must survive.
	leads := []map[string]string{
		{
			"id": "gh-keep", "title": "DevOps Engineer", "company": "acme",
			"description": "desc", "direct_apply_url": "https://example.com/gh-keep",
			"ats_platform_name": "Greenhouse", "scraped_timestamp": "2026-07-01T00:00:00.000Z",
			"location": "Remote", "posted_at": "",
		},
	}

	var boardCalls [][]scraper.BoardRef
	var tenantPlatforms [][]string
	srv := boardAwareWorker(t, &boardCalls, &tenantPlatforms, leads, nil)
	defer srv.Close()

	s := New(scraper.New(srv.URL, 5*time.Second), repo, dataDir, "@daily", nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.EnableGlobalScrape(GlobalScrapeConfig{BoardBatch: 10, DistributeLimit: 100, CloseGraceHours: 24})

	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	open := unscoredIDs(t, repo)
	if !open["gh-flap"] {
		t.Error("gh-flap was closed on a single missed cycle despite the grace window")
	}
	if !open["gh-keep"] {
		t.Error("gh-keep was closed despite being re-seen")
	}
}

// TestGlobalDiffReopensResurfaced: a closed row whose posting shows up on its
// board again is reopened to pending. Without this a false close (flap,
// case-drifted twin) was permanent, because distribution never re-inserts a
// row that already exists.
func TestGlobalDiffReopensResurfaced(t *testing.T) {
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	dataDir := t.TempDir()
	companies := `{"SEARCH_TERMS":["engineer"],"GREENHOUSE_COMPANIES":["acme"]}`
	if err := os.WriteFile(filepath.Join(dataDir, "companies.json"), []byte(companies), 0o644); err != nil {
		t.Fatalf("write companies.json: %v", err)
	}

	if _, err := repo.InsertScrapedLead(seedLead("gh-flap", "SRE Engineer", "acme", "Greenhouse")); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := repo.SetPipelineStage("gh-flap", "closed"); err != nil {
		t.Fatalf("close: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)

	// The posting is back on the board this cycle.
	leads := []map[string]string{
		{
			"id": "gh-flap", "title": "SRE Engineer", "company": "acme",
			"description": "desc", "direct_apply_url": "https://example.com/gh-flap",
			"ats_platform_name": "Greenhouse", "scraped_timestamp": "2026-07-01T00:00:00.000Z",
			"location": "Remote", "posted_at": "",
		},
	}

	var boardCalls [][]scraper.BoardRef
	var tenantPlatforms [][]string
	srv := boardAwareWorker(t, &boardCalls, &tenantPlatforms, leads, nil)
	defer srv.Close()

	s := New(scraper.New(srv.URL, 5*time.Second), repo, dataDir, "@daily", nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.EnableGlobalScrape(GlobalScrapeConfig{BoardBatch: 10, DistributeLimit: 100, CloseGraceHours: 24})

	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	open := unscoredIDs(t, repo)
	if !open["gh-flap"] {
		t.Error("resurfaced posting was not reopened to pending")
	}
}

// TestGlobalDiffSkipsZeroLeadBoards pins the "unknown is not empty" invariant
// (round 1 of the mass-close incident): a board that contributed no leads this
// cycle (blocked page, WAF, failed chunk) never enters the fetched set, so
// none of its held rows may be closed by the diff. A board that did return
// leads on the same cycle still closes its genuinely vanished rows.
func TestGlobalDiffSkipsZeroLeadBoards(t *testing.T) {
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	dataDir := t.TempDir()
	companies := `{"SEARCH_TERMS":["engineer"],"GREENHOUSE_COMPANIES":["acme","otherco"]}`
	if err := os.WriteFile(filepath.Join(dataDir, "companies.json"), []byte(companies), 0o644); err != nil {
		t.Fatalf("write companies.json: %v", err)
	}

	for _, l := range []model.Lead{
		seedLead("acme-keep", "DevOps Engineer", "acme", "Greenhouse"),
		seedLead("acme-gone", "SRE Engineer", "acme", "Greenhouse"),
		seedLead("other-gone", "Platform Engineer", "otherco", "Greenhouse"),
	} {
		if _, err := repo.InsertScrapedLead(l); err != nil {
			t.Fatalf("insert %s: %v", l.ID, err)
		}
	}
	time.Sleep(1100 * time.Millisecond)

	// This cycle the worker returned leads for acme only. otherco produced
	// nothing at all: no lead, no board result, exactly what a silently blocked
	// or failed fetch looks like.
	leads := []map[string]string{
		{
			"id": "acme-keep", "title": "DevOps Engineer", "company": "acme",
			"description": "desc", "direct_apply_url": "https://example.com/acme-keep",
			"ats_platform_name": "Greenhouse", "scraped_timestamp": "2026-07-01T00:00:00.000Z",
			"location": "Remote", "posted_at": "",
		},
	}

	var boardCalls [][]scraper.BoardRef
	var tenantPlatforms [][]string
	srv := boardAwareWorker(t, &boardCalls, &tenantPlatforms, leads, nil)
	defer srv.Close()

	s := New(scraper.New(srv.URL, 5*time.Second), repo, dataDir, "@daily", nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.EnableGlobalScrape(GlobalScrapeConfig{BoardBatch: 10, DistributeLimit: 100})

	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	open := unscoredIDs(t, repo)
	if !open["other-gone"] {
		t.Error("other-gone was closed although its board returned zero leads (unknown must not read as empty)")
	}
	if open["acme-gone"] {
		t.Error("acme-gone survived the diff despite vanishing from a board that did return leads")
	}
	if !open["acme-keep"] {
		t.Error("acme-keep was closed despite being re-seen")
	}
}

// TestGlobalDiffVerifyProbesOnlyWorkday: the pre-close liveness probe is a
// Workday-specific guard (CXS search endpoint). With CloseVerify on, a
// non-Workday close must commit without consulting the prober at all: if the
// platform gate regressed, the probe would come back inconclusive for a
// Greenhouse URL and wrongly skip every Greenhouse close forever.
func TestGlobalDiffVerifyProbesOnlyWorkday(t *testing.T) {
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	dataDir := t.TempDir()
	companies := `{"SEARCH_TERMS":["engineer"],"GREENHOUSE_COMPANIES":["acme"]}`
	if err := os.WriteFile(filepath.Join(dataDir, "companies.json"), []byte(companies), 0o644); err != nil {
		t.Fatalf("write companies.json: %v", err)
	}

	for _, l := range []model.Lead{
		seedLead("gh-keep", "DevOps Engineer", "acme", "Greenhouse"),
		seedLead("gh-gone", "SRE Engineer", "acme", "Greenhouse"),
	} {
		if _, err := repo.InsertScrapedLead(l); err != nil {
			t.Fatalf("insert %s: %v", l.ID, err)
		}
	}
	time.Sleep(1100 * time.Millisecond)

	leads := []map[string]string{
		{
			"id": "gh-keep", "title": "DevOps Engineer", "company": "acme",
			"description": "desc", "direct_apply_url": "https://example.com/gh-keep",
			"ats_platform_name": "Greenhouse", "scraped_timestamp": "2026-07-01T00:00:00.000Z",
			"location": "Remote", "posted_at": "",
		},
	}

	var boardCalls [][]scraper.BoardRef
	var tenantPlatforms [][]string
	srv := boardAwareWorker(t, &boardCalls, &tenantPlatforms, leads, nil)
	defer srv.Close()

	s := New(scraper.New(srv.URL, 5*time.Second), repo, dataDir, "@daily", nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.EnableGlobalScrape(GlobalScrapeConfig{BoardBatch: 10, DistributeLimit: 100, CloseVerify: true})
	s.verifyPost = func(_ context.Context, rawURL string, _ []byte) (*closedcheck.HTTPResult, error) {
		t.Errorf("liveness probe called for a non-Workday close (url %q)", rawURL)
		return &closedcheck.HTTPResult{StatusCode: 403}, nil
	}

	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	open := unscoredIDs(t, repo)
	if open["gh-gone"] {
		t.Error("gh-gone was not closed; the Workday-only probe must not gate Greenhouse closes")
	}
	if !open["gh-keep"] {
		t.Error("gh-keep was closed despite being re-seen")
	}
}

// TestGraceCutoff pins the grace arithmetic's failure mode: anything that
// cannot be shifted (grace off, negative, unparseable cutoff) falls back to
// the unshifted cutoff, meaning close on first miss, never a wider window.
func TestGraceCutoff(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cutoff string
		grace  int
		want   string
	}{
		{"zero grace unchanged", "2026-07-05 12:00:00", 0, "2026-07-05 12:00:00"},
		{"negative grace unchanged", "2026-07-05 12:00:00", -3, "2026-07-05 12:00:00"},
		{"unparseable cutoff unchanged", "garbage", 24, "garbage"},
		{"24h shift", "2026-07-05 12:00:00", 24, "2026-07-04 12:00:00"},
		{"shift crosses midnight", "2026-07-05 03:00:00", 6, "2026-07-04 21:00:00"},
	} {
		if got := graceCutoff(tc.cutoff, tc.grace); got != tc.want {
			t.Errorf("%s: graceCutoff(%q, %d) = %q, want %q", tc.name, tc.cutoff, tc.grace, got, tc.want)
		}
	}
}

// TestDiffCloseVerifierCachesVerdicts: tenants routinely hold the same
// postings, so a definite verdict must probe the board exactly once per cycle,
// including for case-variant URLs of the same posting (the round-4 twins). An
// inconclusive probe must NOT be cached: it may be a transient block and a
// later candidate deserves a retry.
func TestDiffCloseVerifierCachesVerdicts(t *testing.T) {
	calls := map[string]int{}
	post := func(_ context.Context, _ string, body []byte) (*closedcheck.HTTPResult, error) {
		var req struct {
			SearchText string `json:"searchText"`
		}
		_ = json.Unmarshal(body, &req)
		calls[req.SearchText]++
		switch req.SearchText {
		case "JR1":
			return &closedcheck.HTTPResult{StatusCode: 200,
				Body: []byte(`{"total":1,"jobPostings":[{"externalPath":"/job/Remote/Live-Eng_JR1"}]}`)}, nil
		case "JR2":
			return &closedcheck.HTTPResult{StatusCode: 200,
				Body: []byte(`{"total":0,"jobPostings":[]}`)}, nil
		}
		return &closedcheck.HTTPResult{StatusCode: 403, Body: []byte("blocked")}, nil
	}
	v := newDiffCloseVerifier(post)
	ctx := context.Background()

	live := "https://acme.wd5.myworkdayjobs.com/en-US/External/job/Remote/Live-Eng_JR1"
	liveVariant := "https://acme.wd5.myworkdayjobs.com/en-us/EXTERNAL/job/Remote/Live-Eng_JR1"
	for _, u := range []string{live, live, liveVariant} {
		if got := v.check(ctx, u); got != "open" {
			t.Errorf("check(%q) = %q, want open", u, got)
		}
	}
	if calls["JR1"] != 1 {
		t.Errorf("live posting probed %d times, want 1 (verdict cached across case variants)", calls["JR1"])
	}

	dead := "https://acme.wd5.myworkdayjobs.com/en-US/External/job/Remote/Dead-Eng_JR2"
	for i := 0; i < 2; i++ {
		if got := v.check(ctx, dead); got != "closed" {
			t.Errorf("check(dead) = %q, want closed", got)
		}
	}
	if calls["JR2"] != 1 {
		t.Errorf("dead posting probed %d times, want 1 (verdict cached)", calls["JR2"])
	}

	waf := "https://acme.wd5.myworkdayjobs.com/en-US/External/job/Remote/Waf-Eng_JR3"
	for i := 0; i < 2; i++ {
		if got := v.check(ctx, waf); got != "" {
			t.Errorf("check(waf) = %q, want inconclusive", got)
		}
	}
	if calls["JR3"] != 2 {
		t.Errorf("inconclusive posting probed %d times, want 2 (never cached)", calls["JR3"])
	}
}

// wdLead builds a tenant-held Workday row whose URL parses for the pre-close
// liveness probe.
func wdLead(id, title, reqID string) model.Lead {
	return model.Lead{
		JobLead: model.JobLead{
			Title:            title,
			Company:          "MegaCorp",
			Description:      "desc for " + id,
			DirectApplyURL:   "https://megacorp.wd5.myworkdayjobs.com/en-US/External/job/Remote/" + title + "_" + reqID,
			ATSPlatformName:  "Workday",
			ScrapedTimestamp: "2026-07-01T00:00:00.000Z",
			Location:         "Remote",
		},
		ID: id,
	}
}

// TestGlobalDiffVerifiesWorkdayCloses reproduces round 5 of the mass-close
// incident: a truncated Workday listing that reads complete (self-corroborating
// total) omits live postings, and a posting such fetches never re-see goes
// stale past the grace window, where neither grace nor reopen can save it. The
// pre-close probe asks the posting's own board: a live hit vetoes the close and
// stamps the sighting, an inconclusive probe skips the close, and only a clean
// miss commits it.
func TestGlobalDiffVerifiesWorkdayCloses(t *testing.T) {
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	dataDir := t.TempDir()
	companies := `{"SEARCH_TERMS":["engineer"],"WORKDAY_COMPANIES":[{"sub":"megacorp","wd":5,"board":"External","label":"MegaCorp"}]}`
	if err := os.WriteFile(filepath.Join(dataDir, "companies.json"), []byte(companies), 0o644); err != nil {
		t.Fatalf("write companies.json: %v", err)
	}

	for _, l := range []model.Lead{
		wdLead("wd-keep", "Keep-Engineer", "JR300"),
		wdLead("wd-live", "Live-Engineer", "JR100"),
		wdLead("wd-dead", "Dead-Engineer", "JR200"),
		wdLead("wd-waf", "Waf-Engineer", "JR400"),
	} {
		if _, err := repo.InsertScrapedLead(l); err != nil {
			t.Fatalf("insert %s: %v", l.ID, err)
		}
	}
	time.Sleep(1100 * time.Millisecond)
	preCycle := time.Now().UTC().Format("2006-01-02 15:04:05")

	// This cycle's complete-reading fetch listed only wd-keep.
	leads := []map[string]string{
		{
			"id": "wd-keep", "title": "Keep-Engineer", "company": "MegaCorp",
			"description": "desc", "direct_apply_url": "https://megacorp.wd5.myworkdayjobs.com/en-US/External/job/Remote/Keep-Engineer_JR300",
			"ats_platform_name": "Workday", "scraped_timestamp": "2026-07-01T00:00:00.000Z",
			"location": "Remote", "posted_at": "",
		},
	}

	var boardCalls [][]scraper.BoardRef
	var tenantPlatforms [][]string
	srv := boardAwareWorker(t, &boardCalls, &tenantPlatforms, leads, nil)
	defer srv.Close()

	s := New(scraper.New(srv.URL, 5*time.Second), repo, dataDir, "@daily", nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.EnableGlobalScrape(GlobalScrapeConfig{BoardBatch: 10, DistributeLimit: 100, CloseVerify: true})
	s.verifyPost = func(_ context.Context, rawURL string, body []byte) (*closedcheck.HTTPResult, error) {
		if rawURL != "https://megacorp.wd5.myworkdayjobs.com/wday/cxs/megacorp/External/jobs" {
			t.Errorf("probe URL = %q", rawURL)
		}
		var req struct {
			SearchText string `json:"searchText"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.SearchText {
		case "JR100":
			return &closedcheck.HTTPResult{StatusCode: 200,
				Body: []byte(`{"total":1,"jobPostings":[{"externalPath":"/job/Remote/Live-Engineer_JR100"}]}`)}, nil
		case "JR400":
			return &closedcheck.HTTPResult{StatusCode: 403, Body: []byte("blocked")}, nil
		}
		return &closedcheck.HTTPResult{StatusCode: 200, Body: []byte(`{"total":0,"jobPostings":[]}`)}, nil
	}

	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	open := unscoredIDs(t, repo)
	if !open["wd-live"] {
		t.Error("wd-live was closed although its board still serves it")
	}
	if !open["wd-waf"] {
		t.Error("wd-waf was closed on an inconclusive probe")
	}
	if open["wd-dead"] {
		t.Error("wd-dead survived although the probe confirmed it gone")
	}
	if !open["wd-keep"] {
		t.Error("wd-keep was closed despite being re-seen")
	}

	// The veto stamped a verified sighting, so wd-live is no longer a close
	// candidate against a cutoff after the seed harvest.
	vanished, err := repo.VanishedBoardJobs("workday", []string{"megacorp"}, preCycle)
	if err != nil {
		t.Fatalf("VanishedBoardJobs: %v", err)
	}
	for _, v := range vanished {
		if v.ID == "wd-live" {
			t.Error("wd-live still reads vanished; the veto did not stamp last_seen_at")
		}
	}
}

// TestCollectBoardUnionFoldsWorkdayBoardCase: registry harvesting created
// lowercase twins of labeled Workday board entries (intel/External vs
// intel/external); Workday serves the same site for either casing, so the
// union must fetch it once, under the labeled spelling.
func TestCollectBoardUnionFoldsWorkdayBoardCase(t *testing.T) {
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	dataDir := t.TempDir()
	companies := `{"WORKDAY_COMPANIES":[
		{"sub":"intel","wd":1,"board":"external"},
		{"sub":"intel","wd":1,"board":"External","label":"Intel"},
		{"sub":"intel","wd":1,"board":"IntelCareers","label":"Intel"}
	]}`
	if err := os.WriteFile(filepath.Join(dataDir, "companies.json"), []byte(companies), 0o644); err != nil {
		t.Fatalf("write companies.json: %v", err)
	}

	s := New(nil, repo, dataDir, "@daily", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	boards := s.collectBoardUnion(context.Background(), []string{db.LocalUser})

	var external, careers int
	for _, b := range boards {
		switch {
		case b.Platform == "workday" && b.Board == "External" && b.Label == "Intel":
			external++
		case b.Platform == "workday" && b.Board == "external":
			t.Errorf("label-less lowercase twin survived: %+v", b)
		case b.Platform == "workday" && b.Board == "IntelCareers":
			careers++
		}
	}
	if external != 1 {
		t.Errorf("labeled External board count = %d, want 1 (union: %+v)", external, boards)
	}
	if careers != 1 {
		t.Error("distinct board on the same tenant was wrongly folded")
	}
}

// TestCollectBoardUnionFoldsRegistryBoards: verified registry boards live only
// in the shared registry (nothing writes them to a tenant config), so the union
// must pull them in directly even when no tenant lists them. This covers the
// conditional-fetch platforms (Greenhouse/Ashby/Lever) plus Gem and
// SmartRecruiters. Workday is deliberately excluded: no conditional-fetch path
// and 429-prone, so its registry boards must NOT leak into the union here.
func TestCollectBoardUnionFoldsRegistryBoards(t *testing.T) {
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	if err := repo.CompanyRegistry().Upsert(context.Background(), []discovery.VerifiedBoard{
		{Platform: "gem", Slug: "nominal"},
		{Platform: "smartrecruiters", Slug: "Freshworks"},
		{Platform: "greenhouse", Slug: "stripe"},
		{Platform: "ashby", Slug: "fluidstack"},
		{Platform: "lever", Slug: "galvanick"},
		{Platform: "workday", Slug: "acme", Workday: &discovery.WorkdayEntry{Sub: "acme", WD: 1, Board: "External"}},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Empty tenant dir: nothing configured, so any board must come from the registry.
	s := New(nil, repo, t.TempDir(), "@daily", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	boards := s.collectBoardUnion(context.Background(), []string{db.LocalUser})

	var gem, sr, greenhouse, ashby, lever, workday int
	for _, b := range boards {
		switch {
		case b.Platform == "gem" && b.Slug == "nominal":
			gem++
		// SmartRecruiters slugs are case-sensitive for the API call, so the
		// stored spelling must survive the fold unchanged.
		case b.Platform == "smartrecruiters" && b.Slug == "Freshworks":
			sr++
		case b.Platform == "greenhouse" && b.Slug == "stripe":
			greenhouse++
		case b.Platform == "ashby" && b.Slug == "fluidstack":
			ashby++
		case b.Platform == "lever" && b.Slug == "galvanick":
			lever++
		case b.Platform == "workday":
			workday++
		}
	}
	if gem != 1 {
		t.Errorf("registry Gem board count = %d, want 1 (union: %+v)", gem, boards)
	}
	if sr != 1 {
		t.Errorf("registry SmartRecruiters board count = %d, want 1 (union: %+v)", sr, boards)
	}
	if greenhouse != 1 {
		t.Errorf("registry Greenhouse board count = %d, want 1 (union: %+v)", greenhouse, boards)
	}
	if ashby != 1 {
		t.Errorf("registry Ashby board count = %d, want 1 (union: %+v)", ashby, boards)
	}
	if lever != 1 {
		t.Errorf("registry Lever board count = %d, want 1 (union: %+v)", lever, boards)
	}
	// Workday is excluded from the registry fold on purpose.
	if workday != 0 {
		t.Errorf("workday board leaked from registry into union: %+v", boards)
	}
}

// TestFoldWorkdayRegistryRotates: Workday registry boards fold in a bounded
// slice per cycle and a persisted cursor walks the whole list across cycles
// (wrapping), so every board is refreshed without pulling all ~5k at once.
func TestFoldWorkdayRegistryRotates(t *testing.T) {
	entries := []discovery.WorkdayEntry{
		{Sub: "alpha", WD: 1, Board: "External"},
		{Sub: "bravo", WD: 2, Board: "External"},
		{Sub: "charlie", WD: 3, Board: "External"},
		{Sub: "delta", WD: 4, Board: "External"},
		{Sub: "echo", WD: 5, Board: "External"},
	}

	dir := t.TempDir()
	s := New(nil, nil, dir, "@daily", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.EnableGlobalScrape(GlobalScrapeConfig{WorkdayRegistryBatch: 2})

	cycle := func() []string {
		var got []string
		add := func(platform, slug, label string, wd int, board string) {
			got = append(got, slug)
		}
		s.foldWorkdayRegistry(entries, add)
		return got
	}

	seen := map[string]int{}
	for c := 0; c < 3; c++ {
		got := cycle()
		if len(got) != 2 {
			t.Fatalf("cycle %d folded %d boards, want 2 (%v)", c, len(got), got)
		}
		for _, g := range got {
			seen[g]++
		}
	}
	// 3 cycles * 2 = 6 folds over 5 boards: every board covered at least once,
	// exactly one board revisited by the wrap.
	for _, e := range entries {
		if seen[e.Sub] == 0 {
			t.Errorf("board %q never folded across 3 cycles (seen=%v)", e.Sub, seen)
		}
	}
	total := 0
	for _, n := range seen {
		total += n
	}
	if total != 6 {
		t.Errorf("total folds = %d, want 6", total)
	}
}

// TestFoldWorkdayRegistryDisabledAndFoldAll: batch 0 disables the fold; a batch
// at/above the board count folds everything with no cursor file written.
func TestFoldWorkdayRegistryDisabledAndFoldAll(t *testing.T) {
	entries := []discovery.WorkdayEntry{
		{Sub: "alpha", WD: 1, Board: "External"},
		{Sub: "bravo", WD: 2, Board: "External"},
	}
	count := func(batch int) int {
		s := New(nil, nil, t.TempDir(), "@daily", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
		s.EnableGlobalScrape(GlobalScrapeConfig{WorkdayRegistryBatch: batch})
		n := 0
		s.foldWorkdayRegistry(entries, func(_, _, _ string, _ int, _ string) { n++ })
		return n
	}
	if got := count(0); got != 0 {
		t.Errorf("batch 0 folded %d, want 0 (disabled)", got)
	}
	if got := count(10); got != 2 {
		t.Errorf("batch>=len folded %d, want 2 (all)", got)
	}
}

// TestCollectBoardUnionInterleavesRegistryPlatforms: a platform with far more
// registry boards (Greenhouse) must not be front-loaded ahead of smaller
// platforms (Ashby/Lever), or a timed-out cold cycle scrapes the big platform
// and starves the rest. The union must interleave them so a small platform's
// boards appear near the front, not after the entire big platform.
func TestCollectBoardUnionInterleavesRegistryPlatforms(t *testing.T) {
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	var boards []discovery.VerifiedBoard
	for i := 0; i < 50; i++ {
		boards = append(boards, discovery.VerifiedBoard{Platform: "greenhouse", Slug: "gh" + strconv.Itoa(i)})
	}
	boards = append(boards,
		discovery.VerifiedBoard{Platform: "ashby", Slug: "fluidstack"},
		discovery.VerifiedBoard{Platform: "lever", Slug: "galvanick"},
	)
	if err := repo.CompanyRegistry().Upsert(context.Background(), boards); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	s := New(nil, repo, t.TempDir(), "@daily", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	union := s.collectBoardUnion(context.Background(), []string{db.LocalUser})

	idxAshby, idxLever := -1, -1
	for i, b := range union {
		if b.Platform == "ashby" && b.Slug == "fluidstack" {
			idxAshby = i
		}
		if b.Platform == "lever" && b.Slug == "galvanick" {
			idxLever = i
		}
	}
	if idxAshby < 0 || idxLever < 0 {
		t.Fatalf("ashby/lever board missing from union (ashby=%d lever=%d)", idxAshby, idxLever)
	}
	// With 50 greenhouse boards, sequential appending would push ashby/lever to
	// index ~50; interleaving round-robin puts them within the first handful.
	if idxAshby > 5 || idxLever > 5 {
		t.Errorf("registry platforms front-loaded, not interleaved: ashby@%d lever@%d (want <=5)", idxAshby, idxLever)
	}
}
