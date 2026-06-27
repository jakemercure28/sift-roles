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

func TestCalibrateScoreCapsOverLeveledRole(t *testing.T) {
	job := Job{
		Title:       "Principal Engineer, Compute Platform",
		Description: "Requires 12+ years of relevant industry experience with distributed systems.",
	}
	in := Result{Score: intPtr(9), Reasoning: "Strong stack match."}
	got := calibrateScore(job, nineYearResume, in)
	// 12+ required vs ~9 shown is a 3-year shortfall on a 10+ role: capped to 6.
	if got.Score == nil || *got.Score != 6 {
		t.Fatalf("score = %v, want 6 (capped)", ptrStr(got.Score))
	}
	if !strings.Contains(got.Reasoning, "Calibration: JD requires") {
		t.Fatalf("expected calibration note appended, got %q", got.Reasoning)
	}
}

func TestCalibrateScoreLeavesNearMissUnchanged(t *testing.T) {
	// 10+ required vs ~9 shown is a one-year gap: no cap.
	job := Job{Title: "Staff Engineer", Description: "You have 10+ years of professional experience."}
	in := Result{Score: intPtr(9), Reasoning: "On target."}
	got := calibrateScore(job, nineYearResume, in)
	if got.Score == nil || *got.Score != 9 {
		t.Fatalf("score = %v, want 9 (unchanged)", ptrStr(got.Score))
	}
}

func TestCalibrateScoreNoOpWithoutRequirement(t *testing.T) {
	job := Job{Title: "DevOps Engineer", Description: "Work on AWS and Kubernetes. No specific years listed."}
	in := Result{Score: intPtr(9), Reasoning: "On target."}
	got := calibrateScore(job, nineYearResume, in)
	if got.Score == nil || *got.Score != 9 {
		t.Fatalf("score = %v, want 9 (no requirement, no cap)", ptrStr(got.Score))
	}
}

func TestMaxRequiredIgnoresCompanyBackgroundYears(t *testing.T) {
	// The "20 years" describes the company, not a requirement; "12+ ... experience"
	// is the real minimum and must win.
	text := "Our company has 20 years of leadership in the market. Requires 12+ years of relevant industry experience."
	if got := maxRequiredExperienceYears(text); got != 12 {
		t.Fatalf("maxRequiredExperienceYears = %d, want 12", got)
	}
}

func TestMaxRequiredCatchesMorePhrasings(t *testing.T) {
	cases := map[string]int{
		// "requiring" lead-in (previously missed; only "requires" matched).
		"This is a Staff role requiring 15+ years of experience.": 15,
		// Bare "N+ years of experience" with no lead-in and no qualifier word.
		"Responsibilities aside, 12+ years of experience is expected.": 12,
		"You'll have 10+ years of experience in distributed systems.":  10,
		// Slash-delimited qualifier ("6+ years of SRE/DevOps experience"): the slash
		// used to break the filler and drop the requirement to 0.
		"This SRE role wants 6+ years of SRE/DevOps experience.": 6,
		// Hyphenated qualifier ("back-end") must still parse.
		"Requires 8+ years of back-end engineering experience.": 8,
		// Range with no qualifier word ("7-10 years of experience").
		"Looking for 7-10 years of experience building platforms.": 7,
		// Company boast with no + and no qualifier stays ignored (not a requirement).
		"We bring 20 years of experience to every engagement.": 0,
		// Pure background, no requirement at all.
		"A fast-growing team that loves Kubernetes.": 0,
	}
	for text, want := range cases {
		if got := maxRequiredExperienceYears(text); got != want {
			t.Errorf("maxRequiredExperienceYears(%q) = %d, want %d", text, got, want)
		}
	}
}

func TestEstimateIgnoresEducationDateRanges(t *testing.T) {
	resume := "Education\nB.S. Computer Science, University of Example, 2004 - 2008\n\n" +
		"Experience\nEngineer, Jan 2018 - Jan 2021\nBuilt CI/CD pipelines."
	got := estimateCandidateExperienceYears(resume, time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC))
	// Only the 2018–2021 work span (3 years) should count, not the degree's 2004–2008.
	if got < 2.5 || got > 3.5 {
		t.Fatalf("estimateCandidateExperienceYears = %.2f, want ~3 (education range excluded)", got)
	}
}

// TestScoreJobsCalibratesBatchPath drives the full batch path: the model returns a
// 9 for an over-leveled 12+ role, and the deterministic cap must lower it even
// though the score came from the batch (not single-job) path.
func TestScoreJobsCalibratesBatchPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "resume.md"), []byte(nineYearResume), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req geminiRequest
		_ = json.Unmarshal(body, &req)
		// Batch call: return a 9 for the over-leveled job and a 9 for the on-target one.
		writeCandidate(w, `[{"index":0,"score":9,"reasoning":"strong stack"},{"index":1,"score":9,"reasoning":"on target"}]`)
	}))
	defer srv.Close()

	s := New(newTestClient(srv.URL, "k", &fakeUsage{}), dir)
	jobs := []Job{
		{Title: "Principal Engineer", Description: "Requires 12+ years of relevant industry experience."},
		{Title: "DevOps Engineer", Description: "AWS and Kubernetes, no specific years."},
	}

	results, err := s.ScoreJobs(context.Background(), jobs)
	if err != nil {
		t.Fatalf("ScoreJobs: %v", err)
	}
	if results[0].Score == nil || *results[0].Score != 6 {
		t.Fatalf("over-leveled batch score = %v, want 6 (calibrated)", ptrStr(results[0].Score))
	}
	if results[1].Score == nil || *results[1].Score != 9 {
		t.Fatalf("on-target batch score = %v, want 9 (unchanged)", ptrStr(results[1].Score))
	}
}
