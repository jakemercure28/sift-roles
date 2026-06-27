package dashboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"job-search-automation/internal/db"
)

func TestRenderMarketResearchParity(t *testing.T) {
	generatedAt := time.Date(2099, 1, 2, 15, 24, 0, 0, time.UTC).UnixMilli()
	jobCount := 4
	applicantYoe := 4
	currentJobs := []db.MarketSeniorityJob{
		{
			ID: "mr-1", Title: "Platform Engineer", Company: "Acme Cloud", Score: ptr(9),
			Status: "pending", Location: "Remote - San Francisco, CA", PostedAt: "2026-06-01",
			Description: "Build Kubernetes platform automation with Terraform, AWS, CI/CD, and observability. Requires 3 years of experience.",
		},
		{
			ID: "mr-2", Title: "Senior Site Reliability Engineer", Company: "Globex", Score: ptr(8),
			Status: "pending", Location: "Hybrid - Austin, TX", PostedAt: "2026-05-22",
			Description: "Own SLOs, incident response, observability, Linux, Terraform, and Kubernetes. Minimum 5 years experience.",
		},
		{
			ID: "mr-3", Title: "DevOps Engineer", Company: "Initech", Score: ptr(7),
			Status: "applied", Stage: "phone_screen", Location: "New York, NY / Seattle, WA", PostedAt: "2026-05-15",
			Description: "Ship CI/CD, AWS, Docker, Kubernetes, and infrastructure automation. 4 years in production engineering preferred.",
		},
		{
			ID: "mr-4", Title: "Staff Infrastructure Engineer", Company: "Umbrella", Score: ptr(10),
			Status: "pending", Location: "United States", PostedAt: "2026-05-10",
			Description: "Lead AI infrastructure, GPU scheduling, inference serving, Terraform, and Kubernetes. 8 years of relevant experience.",
		},
	}
	allTimeJobs := append([]db.MarketSeniorityJob{}, currentJobs...)
	allTimeJobs = append(allTimeJobs, db.MarketSeniorityJob{
		ID: "mr-old", Title: "Junior Cloud Engineer", Company: "OldCo", Score: ptr(6),
		Status: "closed", Stage: "closed", Location: "Denver, CO", PostedAt: "2026-04-01",
		Description: "Support AWS operations, monitoring, and automation. 1 year of related experience.",
	})

	poleA, poleB := 65.0, 35.0
	page := marketResearchPageData{
		Cache: &marketCache{
			GeneratedAt: generatedAt,
			JobCount:    &jobCount,
			Data: marketAnalysisData{
				SampleSize: 4,
				TopSkills: []marketSkill{
					{Skill: "Kubernetes", Count: 4, Pct: 100},
					{Skill: "Terraform", Count: 3, Pct: 75},
					{Skill: "AWS", Count: 2, Pct: 50},
					{Skill: "Observability", Count: 2, Pct: 50},
					{Skill: "CI/CD", Count: 2, Pct: 50},
					{Skill: "GPU scheduling", Count: 1, Pct: 25},
					{Skill: "Inference serving", Count: 1, Pct: 25},
					{Skill: "Incident response", Count: 1, Pct: 25},
					{Skill: "Linux", Count: 1, Pct: 25},
					{Skill: "Docker", Count: 1, Pct: 25},
					{Skill: "SLOs", Count: 1, Pct: 25},
				},
				GapAnalysis: []marketGap{
					{Skill: "Kubernetes", Count: 4, Pct: 100},
					{Skill: "Terraform", Count: 3, Pct: 75},
					{Skill: "GPU scheduling", Count: 1, Pct: 25},
				},
				ResumeStrengths: []marketStrength{
					{Skill: "AWS", Count: 2},
					{Skill: "CI/CD", Count: 2},
					{Skill: "Observability", Count: 2},
				},
				StrategyScore: &marketStrategy{
					PoleALabel: "Platform Builder",
					PoleBLabel: "Reliability / operations",
					PoleAPct:   &poleA,
					PoleBPct:   &poleB,
				},
				EmergingHighScore: []marketSignal{
					{Term: "GPU scheduling", JobCount: 1},
					{Term: "Inference serving", JobCount: 1},
					{Term: "SLOs", JobCount: 1},
				},
			},
		},
		JobCount:     4,
		AllJobs:      currentJobs,
		ApplicantYoe: &applicantYoe,
		AppliedCount: 1,
		Current:      marketResearchDataSet{JobCount: 4, JDCount: 4, Jobs: currentJobs},
		AllTime:      marketResearchDataSet{JobCount: len(allTimeJobs), JDCount: 5, Jobs: allTimeJobs},
	}

	assertGolden(t, "market-research.html.golden", renderMarketResearch(page))
}

func TestMarketResearchBodyCacheIsTenantScoped(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	seedTenantMarketJob(t, conn, "tenant-a", "tenant-a-job", "Junior Cloud Engineer", "Support AWS operations. 1 year of related experience. "+strings.Repeat("junior platform work. ", 8))
	seedTenantMarketJob(t, conn, "tenant-b", "tenant-b-job", "Staff Infrastructure Engineer", "Lead infrastructure strategy. 8 years of related experience. "+strings.Repeat("staff platform work. ", 8))

	srv, err := New(t.TempDir(), repo.ForUser("tenant-a"), nil, time.Second, 500, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatalf("New tenant A: %v", err)
	}
	htmlA, err := srv.renderMarketResearchBody("")
	if err != nil {
		t.Fatalf("render tenant A: %v", err)
	}

	tenantB := *srv
	tenantB.repo = repo.ForUser("tenant-b")
	gotB, err := tenantB.renderMarketResearchBody("")
	if err != nil {
		t.Fatalf("render tenant B: %v", err)
	}

	freshB, err := New(t.TempDir(), repo.ForUser("tenant-b"), nil, time.Second, 500, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatalf("New fresh tenant B: %v", err)
	}
	wantB, err := freshB.renderMarketResearchBody("")
	if err != nil {
		t.Fatalf("render fresh tenant B: %v", err)
	}

	if gotB != wantB {
		t.Fatal("tenant B received stale market research HTML from the shared cache")
	}
	if gotB == htmlA {
		t.Fatal("tenant B market research HTML matched tenant A unexpectedly")
	}
}

func TestMarketResearchBodyCacheReportsHit(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	seedTenantMarketJob(t, conn, "tenant-a", "tenant-a-job", "Cloud Engineer", "Build AWS operations. 2 years of related experience. "+strings.Repeat("platform work. ", 8))

	srv, err := New(t.TempDir(), repo.ForUser("tenant-a"), nil, time.Second, 500, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, cache, err := srv.renderMarketResearchBodyStatus(""); err != nil || cache != "miss" {
		t.Fatalf("first render cache=%q err=%v, want miss nil", cache, err)
	}
	if _, cache, err := srv.renderMarketResearchBodyStatus(""); err != nil || cache != "hit" {
		t.Fatalf("second render cache=%q err=%v, want hit nil", cache, err)
	}
	if _, cache, err := srv.renderMarketResearchBodyStatus("rerun failed"); err != nil || cache != "bypass" {
		t.Fatalf("analysis error render cache=%q err=%v, want bypass nil", cache, err)
	}
}

func seedTenantMarketJob(t *testing.T, conn *sql.DB, userID, id, title, desc string) {
	t.Helper()
	_, err := conn.Exec(`
		INSERT INTO jobs (user_id, id, title, company, url, description, status, score, created_at, updated_at)
		VALUES (?, ?, ?, 'Acme', 'https://example.com/job', ?, 'pending', 9, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`,
		userID, id, title, desc)
	if err != nil {
		t.Fatalf("seed tenant market job %s: %v", id, err)
	}
}

func TestMarketResearchPostWritesCacheAndRedirects(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "resume.md"), []byte("AWS and CI/CD experience."), 0o644); err != nil {
		t.Fatalf("write resume: %v", err)
	}
	desc := strings.Repeat("Kubernetes Terraform AWS CI/CD observability platform automation. ", 3)
	seedFullJob(t, conn, db.ListedJob{
		ID: "mr-post-1", Title: "Platform Engineer", Company: "Acme",
		Status: "pending", Score: ptr(9), Location: "Remote", Description: desc,
	})

	var promptSeen string
	fake := &fakeRejection{askFn: func(prompt string) (string, error) {
		promptSeen = prompt
		return `{"summary":"1 of 1 JDs mention Kubernetes.","top_skills":[{"skill":"Kubernetes","count":1,"pct":100}],"gap_analysis":[],"resume_strengths":[{"skill":"AWS","count":1}],"trending":[],"location_breakdown":{"remote":1,"hybrid":0,"in_person":0,"not_specified":0,"top_cities":[]},"sample_size":1,"strategy_score":{"pole_a_label":"Platform Builder","pole_b_label":"Operations","pole_a_pct":60,"pole_b_pct":40},"emerging_high_score":[]}`, nil
	}}
	srv, err := New(t.TempDir(), repo, fake, time.Second, 500, dataDir, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp := postNoRedirect(t, ts.URL+"/market-research")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/?filter=market-research" {
		t.Fatalf("Location = %q", loc)
	}
	if !strings.Contains(promptSeen, "Analyze these 1 job descriptions") || !strings.Contains(promptSeen, "[JD 1] Acme — Platform Engineer") {
		t.Fatalf("prompt missing expected content:\n%s", promptSeen)
	}

	data, err := os.ReadFile(filepath.Join(dataDir, "market-research-cache.json"))
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var cache marketCache
	if err := json.Unmarshal(data, &cache); err != nil {
		t.Fatalf("cache json: %v", err)
	}
	if cache.JobCount == nil || *cache.JobCount != 1 || len(cache.Data.TopSkills) != 1 {
		t.Fatalf("cache = %+v", cache)
	}
}

func TestMarketResearchPostQuotaErrorRedirectsWithoutCache(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	dataDir := t.TempDir()
	desc := strings.Repeat("Kubernetes Terraform AWS CI/CD observability platform automation. ", 3)
	seedFullJob(t, conn, db.ListedJob{
		ID: "mr-post-err", Title: "Platform Engineer", Company: "Acme",
		Status: "pending", Score: ptr(9), Location: "Remote", Description: desc,
	})
	fake := &fakeRejection{askFn: func(string) (string, error) {
		return "", errors.New("RESOURCE_EXHAUSTED quota")
	}}
	srv, err := New(t.TempDir(), repo, fake, time.Second, 500, dataDir, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp := postNoRedirect(t, ts.URL+"/market-research")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "/?filter=market-research&analysisError=") || !strings.Contains(loc, "Gemini+free-tier+daily+limit") {
		t.Fatalf("Location = %q", loc)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "market-research-cache.json")); !os.IsNotExist(err) {
		t.Fatalf("cache write on quota error err=%v", err)
	}
}

func TestRefreshMarketResearchMaintenanceFailureBacksOff(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	dataDir := t.TempDir()
	desc := strings.Repeat("Kubernetes Terraform AWS CI/CD observability platform automation. ", 3)
	seedFullJob(t, conn, db.ListedJob{
		ID: "mr-maint-err", Title: "Platform Engineer", Company: "Acme",
		Status: "pending", Score: ptr(9), Location: "Remote", Description: desc,
	})

	fake := &fakeRejection{askFn: func(string) (string, error) {
		return "", errors.New("invalid Gemini JSON")
	}}
	srv, err := New(t.TempDir(), repo, fake, time.Second, 500, dataDir, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := srv.RefreshMarketResearch(context.Background(), false); err == nil {
		t.Fatal("RefreshMarketResearch error = nil, want failure")
	}

	cache := loadMarketResearchCache(dataDir)
	if cache == nil || cache.LastAttemptAt == 0 || cache.GeneratedAt != 0 {
		t.Fatalf("failure cache = %+v, want last-attempt sentinel", cache)
	}

	called := false
	fake.askFn = func(string) (string, error) {
		called = true
		return `{}`, nil
	}
	res, err := srv.RefreshMarketResearch(context.Background(), false)
	if err != nil {
		t.Fatalf("RefreshMarketResearch backoff: %v", err)
	}
	if !res.Skipped || res.Reason != "recent_failure_backoff" || res.JobCount != 1 {
		t.Fatalf("refresh = %+v, want recent-failure backoff", res)
	}
	if called {
		t.Fatal("Gemini was called despite recent failure backoff")
	}
}

func TestRefreshMarketResearchSkipsFreshCache(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	dataDir := t.TempDir()
	desc := strings.Repeat("Kubernetes Terraform AWS CI/CD observability platform automation. ", 3)
	seedFullJob(t, conn, db.ListedJob{
		ID: "mr-fresh-cache", Title: "Platform Engineer", Company: "Acme",
		Status: "pending", Score: ptr(9), Location: "Remote", Description: desc,
	})
	jobCount := 1
	cache := marketCache{
		GeneratedAt: time.Now().UnixMilli(),
		JobCount:    &jobCount,
		Data: marketAnalysisData{
			Summary:    "cached",
			SampleSize: 1,
		},
	}
	raw, err := marshalIndentNoHTMLEscape(cache)
	if err != nil {
		t.Fatalf("marshal cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "market-research-cache.json"), raw, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	called := false
	fake := &fakeRejection{askFn: func(string) (string, error) {
		called = true
		return `{}`, nil
	}}
	srv, err := New(t.TempDir(), repo, fake, time.Second, 500, dataDir, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := srv.RefreshMarketResearch(context.Background(), false)
	if err != nil {
		t.Fatalf("RefreshMarketResearch: %v", err)
	}
	if !res.Skipped || res.Reason != "cache_fresh" || res.JobCount != 1 {
		t.Fatalf("refresh = %+v, want fresh-cache skip", res)
	}
	if called {
		t.Fatal("Gemini was called despite fresh cache")
	}
}

func TestRefreshMarketResearchForceBypassesFreshCache(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	dataDir := t.TempDir()
	desc := strings.Repeat("Kubernetes Terraform AWS CI/CD observability platform automation. ", 3)
	seedFullJob(t, conn, db.ListedJob{
		ID: "mr-force-cache", Title: "Platform Engineer", Company: "Acme",
		Status: "pending", Score: ptr(9), Location: "Remote", Description: desc,
	})
	jobCount := 1
	cache := marketCache{GeneratedAt: time.Now().UnixMilli(), JobCount: &jobCount}
	raw, err := marshalIndentNoHTMLEscape(cache)
	if err != nil {
		t.Fatalf("marshal cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "market-research-cache.json"), raw, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	called := false
	fake := &fakeRejection{askFn: func(string) (string, error) {
		called = true
		return `{"summary":"fresh run","top_skills":[],"gap_analysis":[],"resume_strengths":[],"trending":[],"location_breakdown":{"top_cities":[]},"sample_size":1,"emerging_high_score":[]}`, nil
	}}
	srv, err := New(t.TempDir(), repo, fake, time.Second, 500, dataDir, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := srv.RefreshMarketResearch(context.Background(), true)
	if err != nil {
		t.Fatalf("RefreshMarketResearch: %v", err)
	}
	if res.Skipped || res.JobCount != 1 {
		t.Fatalf("refresh = %+v, want force refresh", res)
	}
	if !called {
		t.Fatal("Gemini was not called on force refresh")
	}
}

func postNoRedirect(t *testing.T, url string) *http.Response {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Post(url, "application/x-www-form-urlencoded", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}
