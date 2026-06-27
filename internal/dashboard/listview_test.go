package dashboard

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"job-search-automation/internal/db"
)

// seedFullJob inserts a job row with arbitrary fields for list-view tests.
func seedFullJob(t *testing.T, conn *sql.DB, j db.ListedJob) {
	t.Helper()
	var score any
	if j.Score != nil {
		score = *j.Score
	}
	_, err := conn.Exec(`INSERT INTO jobs
		(id, title, company, location, posted_at, created_at, updated_at, description,
		 score, reasoning, status, stage, applied_at, rejected_from_stage, rejected_at, platform)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.Title, j.Company, j.Location, j.PostedAt, j.CreatedAt, j.UpdatedAt, j.Description,
		score, j.Reasoning, j.Status, nullIfEmpty(j.Stage), nullIfEmpty(j.AppliedAt),
		nullIfEmpty(j.RejectedFromStage), nullIfEmpty(j.RejectedAt), j.Platform)
	if err != nil {
		t.Fatalf("seed %s: %v", j.ID, err)
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func dataLayerRepo(t *testing.T) (*db.Repository, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.db")
	repo, err := db.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { repo.Close() })
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return repo, conn
}

func ids(jobs []db.ListedJob) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.ID
	}
	return out
}

func TestGlobalStatsAndComputeStatsAgree(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	seedFullJob(t, conn, db.ListedJob{ID: "a", Status: "pending"})
	seedFullJob(t, conn, db.ListedJob{ID: "b", Status: "applied", Stage: "phone_screen"})
	seedFullJob(t, conn, db.ListedJob{ID: "c", Status: "applied", Stage: "offer"})
	seedFullJob(t, conn, db.ListedJob{ID: "d", Status: "rejected", Stage: "rejected"})
	seedFullJob(t, conn, db.ListedJob{ID: "e", Status: "archived"})

	stats, err := repo.GlobalStats()
	if err != nil {
		t.Fatalf("GlobalStats: %v", err)
	}
	// total excludes archived/rejected: a,b,c => 3. notApplied: a => 1.
	// applied (status applied/responded, stage!=closed): b,c => 2.
	// interviewing (phone_screen,interview,onsite,offer): b,c => 2. offers: c => 1.
	want := db.Stats{Total: 3, NotApplied: 1, Applied: 2, Interviewing: 2, Offers: 1, Rejected: 1, Archived: 1}
	if stats != want {
		t.Fatalf("GlobalStats = %+v, want %+v", stats, want)
	}

	all, err := repo.FilteredJobs("__noop_all", "score DESC")
	_ = all
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	// computeStatsFromJobs over every row must match GlobalStats.
	everything, _ := repo.FilteredJobs("all", "created_at DESC")
	rejected, _ := repo.FilteredJobs("rejected", "score DESC, posted_at DESC, created_at DESC")
	archived, _ := repo.FilteredJobs("archived", "score DESC, posted_at DESC, created_at DESC")
	combined := append(append(append([]db.ListedJob{}, everything...), rejected...), archived...)
	cs := computeStatsFromJobs(combined)
	if cs != want {
		t.Fatalf("computeStatsFromJobs = %+v, want %+v", cs, want)
	}
}

func TestFilteredJobsScoping(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	seedFullJob(t, conn, db.ListedJob{ID: "pending", Status: "pending", Score: ptr(9)})
	seedFullJob(t, conn, db.ListedJob{ID: "applied", Status: "applied", Stage: "applied", Score: ptr(8)})
	seedFullJob(t, conn, db.ListedJob{ID: "offer", Status: "applied", Stage: "offer", Score: ptr(7)})
	seedFullJob(t, conn, db.ListedJob{ID: "rej", Status: "rejected", Stage: "rejected", Score: ptr(6)})
	seedFullJob(t, conn, db.ListedJob{ID: "arch", Status: "archived", Score: ptr(5)})

	orderBy := "score DESC, posted_at DESC, created_at DESC"
	cases := map[string][]string{
		"all":          {"pending", "applied", "offer"},
		"applied":      {"applied", "offer"},
		"offers":       {"offer"},
		"rejected":     {"rej"},
		"archived":     {"arch"},
		"interviewing": {"offer"},
	}
	for filter, want := range cases {
		got, err := repo.FilteredJobs(filter, orderBy)
		if err != nil {
			t.Fatalf("%s: %v", filter, err)
		}
		if !equalStrs(ids(got), want) {
			t.Errorf("filter %q = %v, want %v", filter, ids(got), want)
		}
	}
}

func TestFetchFilteredJobsDateSort(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	seedFullJob(t, conn, db.ListedJob{ID: "old", Status: "pending", Score: ptr(6), PostedAt: "2026-05-01"})
	seedFullJob(t, conn, db.ListedJob{ID: "new", Status: "pending", Score: ptr(8), PostedAt: "2026-06-01"})
	seedFullJob(t, conn, db.ListedJob{ID: "unscored-new", Status: "pending", PostedAt: "2026-06-15"})
	seedFullJob(t, conn, db.ListedJob{ID: "mid", Status: "pending", Score: ptr(7), PostedAt: "2026-05-15"})

	got, err := fetchFilteredJobs(repo, "all", "date")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if want := []string{"new", "mid", "old", "unscored-new"}; !equalStrs(ids(got), want) {
		t.Fatalf("date sort = %v, want %v", ids(got), want)
	}
}

func TestFetchFilteredJobsScoreSortPutsUnscoredLast(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	seedFullJob(t, conn, db.ListedJob{ID: "unscored-new", Status: "pending", PostedAt: "2026-06-20", CreatedAt: "2026-06-20"})
	seedFullJob(t, conn, db.ListedJob{ID: "score-7", Status: "pending", Score: ptr(7), PostedAt: "2026-06-01", CreatedAt: "2026-06-01"})
	seedFullJob(t, conn, db.ListedJob{ID: "score-9", Status: "pending", Score: ptr(9), PostedAt: "2026-05-01", CreatedAt: "2026-05-01"})
	seedFullJob(t, conn, db.ListedJob{ID: "unscored-old", Status: "pending", PostedAt: "2026-04-01", CreatedAt: "2026-04-01"})

	got, err := fetchFilteredJobs(repo, "all", "score")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if want := []string{"score-9", "score-7", "unscored-new", "unscored-old"}; !equalStrs(ids(got), want) {
		t.Fatalf("score sort = %v, want %v", ids(got), want)
	}
}

func TestFetchFilteredJobsLocationSort(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	seedFullJob(t, conn, db.ListedJob{ID: "z", Status: "pending", Location: "Seattle, WA", Score: ptr(5)})
	seedFullJob(t, conn, db.ListedJob{ID: "a", Status: "pending", Location: "Austin, TX", Score: ptr(5)})
	seedFullJob(t, conn, db.ListedJob{ID: "blank", Status: "pending", Location: "", Score: ptr(5)})

	asc, _ := fetchFilteredJobs(repo, "all", "location-asc")
	if want := []string{"a", "z", "blank"}; !equalStrs(ids(asc), want) {
		t.Fatalf("location-asc = %v, want %v (blanks last)", ids(asc), want)
	}
	desc, _ := fetchFilteredJobs(repo, "all", "location-desc")
	if want := []string{"z", "a", "blank"}; !equalStrs(ids(desc), want) {
		t.Fatalf("location-desc = %v, want %v (blanks last)", ids(desc), want)
	}
}

func TestApplyDashboardSearch(t *testing.T) {
	jobs := []db.ListedJob{
		{ID: "1", Title: "Platform Engineer", Company: "Acme", Score: ptr(9)},
		{ID: "2", Title: "Frontend Dev", Company: "Globex", Score: ptr(4)},
		{ID: "3", Title: "SRE", Company: "Acme", Score: nil},
	}
	// Text query matches company "acme" (case-insensitive).
	got := applyDashboardSearch(jobs, SearchOptions{Q: "ACME"})
	if want := []string{"1", "3"}; !equalStrs(ids(got), want) {
		t.Fatalf("text search = %v, want %v", ids(got), want)
	}
	// Min-score floor drops scored jobs below it; null score is kept.
	got2 := applyDashboardSearch(jobs, SearchOptions{MinScore: 5})
	if want := []string{"1", "3"}; !equalStrs(ids(got2), want) {
		t.Fatalf("minScore = %v, want %v", ids(got2), want)
	}
}

func TestPaginateJobs(t *testing.T) {
	var jobs []db.ListedJob
	for i := 0; i < 60; i++ {
		jobs = append(jobs, db.ListedJob{ID: string(rune('a' + i%26))})
	}
	page2, p := paginateJobs(jobs, 2)
	if len(page2) != 25 {
		t.Fatalf("page 2 len = %d, want 25", len(page2))
	}
	if p.TotalPages != 3 || p.Page != 2 || p.StartItem != 26 || p.EndItem != 50 {
		t.Fatalf("pagination = %+v", p)
	}
	// Clamp beyond the last page.
	_, p3 := paginateJobs(jobs, 99)
	if p3.Page != 3 || p3.EndItem != 60 {
		t.Fatalf("clamp = %+v", p3)
	}
	// Empty list still reports one page, zero start.
	_, pe := paginateJobs(nil, 1)
	if pe.TotalPages != 1 || pe.StartItem != 0 || pe.EndItem != 0 {
		t.Fatalf("empty = %+v", pe)
	}
}

func TestFetchFilteredJobsLightPageScoreSort(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	for i := 0; i < 30; i++ {
		n := i
		seedFullJob(t, conn, db.ListedJob{
			ID:        fmt.Sprintf("job-%02d", i),
			Status:    "pending",
			Score:     &n,
			CreatedAt: fmt.Sprintf("2026-06-%02d", i+1),
		})
	}

	jobs, pagination, err := fetchFilteredJobsLightPage(repo, "all", "score", SearchOptions{Page: 2})
	if err != nil {
		t.Fatalf("fetch page: %v", err)
	}
	if pagination.Page != 2 || pagination.TotalItems != 30 || pagination.TotalPages != 2 || pagination.StartItem != 26 || pagination.EndItem != 30 {
		t.Fatalf("pagination = %+v", pagination)
	}
	if len(jobs) != 5 {
		t.Fatalf("page len = %d, want 5", len(jobs))
	}
	if want := []string{"job-04", "job-03", "job-02", "job-01", "job-00"}; !equalStrs(ids(jobs), want) {
		t.Fatalf("page ids = %v, want %v", ids(jobs), want)
	}
}

func TestFetchFilteredJobsLightPageMinScoreKeepsNullScores(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	seedFullJob(t, conn, db.ListedJob{ID: "low", Status: "pending", Score: ptr(2)})
	seedFullJob(t, conn, db.ListedJob{ID: "high", Status: "pending", Score: ptr(8)})
	seedFullJob(t, conn, db.ListedJob{ID: "null", Status: "pending"})

	jobs, pagination, err := fetchFilteredJobsLightPage(repo, "all", "score", SearchOptions{MinScore: 5})
	if err != nil {
		t.Fatalf("fetch page: %v", err)
	}
	if pagination.TotalItems != 2 || pagination.TotalPages != 1 || pagination.Page != 1 {
		t.Fatalf("pagination = %+v", pagination)
	}
	if want := []string{"high", "null"}; !equalStrs(ids(jobs), want) {
		t.Fatalf("minScore ids = %v, want %v", ids(jobs), want)
	}
}

func TestDashboardBodyUsesSQLPagingWhenOnlyIncludeUnknownDisabled(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	for i := 0; i < 30; i++ {
		score := 9
		seedFullJob(t, conn, db.ListedJob{
			ID:     fmt.Sprintf("job-%02d", i),
			Status: "pending",
			Score:  &score,
		})
	}
	srv, err := New(t.TempDir(), repo, nil, 0, 500, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, pagination, cache, err := srv.dashboardBody("all", "score", SearchOptions{}, LocationPrefs{Metros: []string{}, IncludeUnknown: false})
	if err != nil {
		t.Fatalf("dashboardBody: %v", err)
	}
	if cache != "paged" {
		t.Fatalf("cache label = %q, want paged", cache)
	}
	if pagination == nil || pagination.TotalItems != 30 || pagination.Page != 1 || pagination.EndItem != 25 {
		t.Fatalf("pagination = %+v", pagination)
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
