package scorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildExtractPromptIsResumeFreeAndJobLast(t *testing.T) {
	prompt := buildExtractPrompt(Job{Title: "Staff Nurse", Company: "Clinic", Description: "RN license required."})
	if strings.Contains(strings.ToLower(prompt), "resume") {
		t.Fatal("extract prompt must never reference a resume; extracts are tenant-independent")
	}
	if !strings.HasSuffix(prompt, "RN license required.") {
		t.Fatalf("extract prompt should end with the variable job block, got tail %q", prompt[len(prompt)-40:])
	}
	// Static prefix must be byte-identical across jobs (prefix caching).
	other := buildExtractPrompt(Job{Title: "Welder", Company: "Yard", Description: "MIG experience."})
	cut := strings.Index(prompt, "## Job Listing")
	if cut < 0 || !strings.HasPrefix(other, prompt[:cut]) {
		t.Fatal("extract prompt static prefix varies across jobs")
	}
}

func TestExtractJobParsesAndNormalizes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCandidate(w, "```json\n{\"field_function\":\"nursing\",\"core_work\":\"patient care\",\"min_years\":3,\"seniority\":\"mid\",\"location_policy\":\"on-site, Seattle WA\",\"compensation\":\"\",\"summary\":\"ICU team.\"}\n```")
	}))
	defer srv.Close()

	ex := NewExtractor(newTestClient(srv.URL, "k", &fakeUsage{}))
	facts, err := ex.ExtractJob(context.Background(), Job{Title: "Nurse"})
	if err != nil {
		t.Fatalf("ExtractJob: %v", err)
	}
	// Absent arrays normalize to empty lists so stored JSON is stable.
	if !strings.Contains(facts, `"required_credentials":[]`) || !strings.Contains(facts, `"field_function":"nursing"`) {
		t.Fatalf("facts = %s", facts)
	}
}

func TestExtractJobRejectsUnparsable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCandidate(w, "not json")
	}))
	defer srv.Close()

	ex := NewExtractor(newTestClient(srv.URL, "k", &fakeUsage{}))
	if _, err := ex.ExtractJob(context.Background(), Job{Title: "Nurse"}); err == nil {
		t.Fatal("want error on unparsable extract output")
	}
}
