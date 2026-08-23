package scorer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A resume with fixed (non-"present") dates so candidate-year estimates are
// deterministic regardless of when the test runs: Jan 2010 – Jan 2019 = 9 years.
const nineYearResume = "Senior DevOps Engineer\nAcme Corp, Jan 2010 - Jan 2019\nBuilt AWS/Kubernetes platforms."

func TestEstimateIgnoresEducationDateRanges(t *testing.T) {
	resume := "Education\nB.S. Computer Science, University of Example, 2004 - 2008\n\n" +
		"Experience\nEngineer, Jan 2018 - Jan 2021\nBuilt CI/CD pipelines."
	got := estimateCandidateExperienceYears(resume, time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC))
	// Only the 2018–2021 work span (3 years) should count, not the degree's 2004–2008.
	if got < 2.5 || got > 3.5 {
		t.Fatalf("estimateCandidateExperienceYears = %.2f, want ~3 (education range excluded)", got)
	}
}

// The computed years figure must reach the model. It previously did not, so the
// model redid the date math per job and disagreed with itself across jobs scored
// the same day (one said "4 years", another "3 years and 10 months", same resume).
func TestPromptStatesComputedExperienceYears(t *testing.T) {
	single := buildScorePrompt(nineYearResume, 9.0, Job{Title: "SRE"})
	batch := buildBatchPrompt(nineYearResume, 9.0, []Job{{Title: "SRE"}})
	for name, prompt := range map[string]string{"single": single, "batch": batch} {
		if !strings.Contains(prompt, "about 9.0 years") {
			t.Errorf("%s prompt does not state the computed years figure", name)
		}
		if !strings.Contains(prompt, "Do not recompute it") {
			t.Errorf("%s prompt does not forbid recomputing the figure", name)
		}
	}
}

// A resume with no datable history must not assert a bogus 0-year figure.
func TestPromptOmitsYearsWhenUndatable(t *testing.T) {
	prompt := buildScorePrompt("Engineer. No dates anywhere.", 0, Job{Title: "SRE"})
	if strings.Contains(prompt, "Total professional experience") {
		t.Fatal("prompt asserted an experience figure with no datable history")
	}
}

// Years must be a soft signal in the rubric, never a hard cap. A stated minimum
// is the employer's aspiration, and real screens advance candidates under it.
func TestRubricTreatsYearsAsSoftSignal(t *testing.T) {
	if strings.Contains(scoreRubric, "below the role's stated minimum years") {
		t.Error("rubric still hard-caps on a years shortfall")
	}
	for _, want := range []string{
		"Years of experience are NEVER a hard cap",
		"never hard-cap",
		"treat the LOW end",
	} {
		if !strings.Contains(scoreRubric, want) {
			t.Errorf("rubric missing soft-years rule %q", want)
		}
	}
}

// The batch path must pass the model's score through untouched. A deterministic
// years cap used to run here and could only ever lower a score, so a wrong cap
// had no counterweight.
func TestScoreJobsPassesModelScoreThrough(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "resume.md"), []byte(nineYearResume), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req geminiRequest
		_ = json.Unmarshal(body, &req)
		writeCandidate(w, `[{"job_id":"j0","score":9,"reasoning":"strong stack"},{"job_id":"j1","score":9,"reasoning":"on target"}]`)
	}))
	defer srv.Close()

	s := New(newTestClient(srv.URL, "k", &fakeUsage{}), dir)
	jobs := []Job{
		// 12+ asked, ~9 shown. Formerly capped to 6; now the model's 9 stands.
		{Title: "Principal Engineer", Description: "Requires 12+ years of relevant industry experience."},
		{Title: "DevOps Engineer", Description: "AWS and Kubernetes, no specific years."},
	}

	results, err := s.ScoreJobs(context.Background(), jobs)
	if err != nil {
		t.Fatalf("ScoreJobs: %v", err)
	}
	for i, r := range results {
		if r.Score == nil || *r.Score != 9 {
			t.Fatalf("results[%d].Score = %v, want 9 (uncapped)", i, ptrStr(r.Score))
		}
		if strings.Contains(r.Reasoning, "Calibration:") {
			t.Fatalf("results[%d] still carries a calibration note: %q", i, r.Reasoning)
		}
	}
}
