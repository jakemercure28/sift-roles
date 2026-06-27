package dashboard

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"job-search-automation/internal/db"
)

func marketAuditFixtureJobs() []db.MarketSeniorityJob {
	return []db.MarketSeniorityJob{
		{
			ID: "audit-1", Title: "Platform Engineer", Company: "Acme Cloud", Score: ptr(9),
			Status: "pending", Location: "Remote", PostedAt: "2026-06-01",
			Description: "Build AWS platform services with Kubernetes, Terraform, CI/CD, and observability for production teams.",
		},
		{
			ID: "audit-2", Title: "SRE", Company: "Globex", Score: ptr(8),
			Status: "pending", Location: "Hybrid", PostedAt: "2026-05-22",
			Description: "Own incident response, SLOs, Terraform modules, Kubernetes operations, and CI/CD automation.",
		},
		{
			ID: "audit-3", Title: "Infrastructure Engineer", Company: "Initech", Score: ptr(7),
			Status: "pending", Location: "United States", PostedAt: "2026-05-15",
			Description: "Maintain Linux systems, Docker deployments, monitoring, and release automation for internal services.",
		},
	}
}

func TestBuildMarketResearchAuditParity(t *testing.T) {
	generatedAt := time.Date(2099, 1, 2, 15, 24, 0, 0, time.UTC).UnixMilli()
	jobCount := 2
	jobs := marketAuditFixtureJobs()
	cache := &marketCache{
		GeneratedAt: generatedAt,
		JobCount:    &jobCount,
		Data: marketAnalysisData{
			TopSkills: []marketSkill{
				{Skill: "Kubernetes", Count: 2, Pct: 67},
				{Skill: "Terraform", Count: 2, Pct: 67},
				{Skill: "AWS", Count: 1, Pct: 33},
				{Skill: "CI/CD", Count: 2, Pct: 67},
			},
			GapAnalysis: []marketGap{
				{Skill: "Kubernetes", Count: 2, Pct: 67},
				{Skill: "Terraform", Count: 2, Pct: 67},
			},
		},
	}
	got, err := marshalNoHTMLEscape(buildMarketResearchAudit(jobs, jobs, 4, cache, "AWS and CI/CD experience.", generatedAt))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertGolden(t, "market-research-audit.json.golden", string(got))
}

func TestMarketResearchAuditRouteNative(t *testing.T) {
	ts, _, conn := fragmentServer(t)
	desc := strings.Repeat("Kubernetes Terraform AWS CI/CD observability. ", 4)
	seedFullJob(t, conn, db.ListedJob{
		ID: "route-audit-1", Title: "Platform Engineer", Company: "Acme",
		Status: "pending", Score: ptr(9), Location: "Remote", Description: desc,
	})
	resp := get(t, ts.URL+"/api/market-research/audit", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	var body struct {
		Sample struct {
			CurrentCount int `json:"currentCount"`
		} `json:"sample"`
		Location struct {
			RemotePct int `json:"remotePct"`
		} `json:"location"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Sample.CurrentCount != 1 || body.Location.RemotePct != 100 {
		t.Fatalf("audit body = %+v", body)
	}
}
