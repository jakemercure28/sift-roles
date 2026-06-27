package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"job-search-automation/internal/db"
)

func setupWriteServer(t *testing.T, dataDir string, rejection RejectionScorer) *httptest.Server {
	t.Helper()
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	srv, err := New(t.TempDir(), repo, rejection, time.Second, 500, dataDir, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Stub the scrape trigger so /api/setup/run-refresh can actually fire.
	trigger := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(trigger.Close)
	srv.SetScrapeTriggerURL(trigger.URL)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestSetupRunRefreshTriggersScrape(t *testing.T) {
	dataDir := t.TempDir()
	var hits int32
	trigger := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer trigger.Close()

	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	srv, err := New(t.TempDir(), repo, nil, time.Second, 500, dataDir, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.SetScrapeTriggerURL(trigger.URL)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Incomplete profile: 400, and the scrape must not fire.
	postJSONStatus(t, ts.URL+"/api/setup/run-refresh", `{}`, http.StatusBadRequest)
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("trigger fired with incomplete profile (hits=%d)", got)
	}

	// Complete the profile, then run-refresh should actually start a scrape.
	for _, f := range []string{"resume.md", "companies.json"} {
		if err := os.WriteFile(filepath.Join(dataDir, f), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	postJSONStatus(t, ts.URL+"/api/setup/run-refresh", `{}`, http.StatusOK)
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("trigger hits = %d, want 1", got)
	}
}

func TestSetupWriteProfileCompaniesAndRefreshNative(t *testing.T) {
	dataDir := t.TempDir()
	ts := setupWriteServer(t, dataDir, nil)

	postJSONStatus(t, ts.URL+"/api/setup/resume", `{"content":"Resume text"}`, http.StatusOK)
	postJSONStatus(t, ts.URL+"/api/setup/profile", `{"titles":"Platform Engineer\nSRE","searchTerms":"Platform\nSRE\nplatform","stack":"Go\nKubernetes","salary":"180000","location":"Remote","industry":"tech"}`, http.StatusOK)

	if got := readTextFileSafe(filepath.Join(dataDir, "resume.md")); got != "Resume text" {
		t.Fatalf("resume = %q", got)
	}
	if got := readTextFileSafe(filepath.Join(dataDir, "context.md")); got != "" {
		t.Fatalf("context.md should not be written by setup, got %q", got)
	}
	if got := readTextFileSafe(filepath.Join(dataDir, "companies.json")); !strings.Contains(got, `"platform"`) || strings.Count(got, `"platform"`) != 1 {
		t.Fatalf("companies.json = %q", got)
	}
	if got := readTextFileSafe(filepath.Join(dataDir, ".onboarded")); strings.TrimSpace(got) == "" {
		t.Fatal("missing onboarded marker")
	}

	postJSONStatus(t, ts.URL+"/api/setup/companies", `{"searchTerms":"backend\ninfra","maxAgeDays":30}`, http.StatusOK)
	if got := readTextFileSafe(filepath.Join(dataDir, "companies.json")); !strings.Contains(got, `"MAX_AGE_DAYS": 30`) || !strings.Contains(got, `"backend"`) {
		t.Fatalf("companies override = %q", got)
	}
	postJSONStatus(t, ts.URL+"/api/setup/run-refresh", `{}`, http.StatusOK)
}

func TestSetupProfileDerivesSearchTermsFromResume(t *testing.T) {
	dataDir := t.TempDir()
	ts := setupWriteServer(t, dataDir, nil)

	postJSONStatus(t, ts.URL+"/api/setup/resume", `{"content":"Visual Merchandiser - Nordstrom\nRetail merchandising and assortment planning."}`, http.StatusOK)
	postJSONStatus(t, ts.URL+"/api/setup/profile", `{}`, http.StatusOK)

	got := readTextFileSafe(filepath.Join(dataDir, "companies.json"))
	if !strings.Contains(got, `"visual merchandiser"`) || !strings.Contains(got, `"merchandising"`) {
		t.Fatalf("derived companies.json = %q", got)
	}
	if got := readTextFileSafe(filepath.Join(dataDir, ".onboarded")); strings.TrimSpace(got) == "" {
		t.Fatal("missing onboarded marker")
	}
}

func TestSetupProfileRequiresResume(t *testing.T) {
	dataDir := t.TempDir()
	ts := setupWriteServer(t, dataDir, nil)

	postJSONStatus(t, ts.URL+"/api/setup/profile", `{"searchTerms":"platform"}`, http.StatusBadRequest)
	if got := readTextFileSafe(filepath.Join(dataDir, ".onboarded")); got != "" {
		t.Fatalf("profile without resume wrote marker: %q", got)
	}
}

func TestSetupSettingsAndApiKeyWritesEnvNative(t *testing.T) {
	dataDir := t.TempDir()
	root := t.TempDir()
	t.Chdir(root)
	ts := setupWriteServer(t, dataDir, nil)

	postJSONStatus(t, ts.URL+"/api/setup/api-key", `{"key":"abc123"}`, http.StatusOK)
	if got := readTextFileSafe(filepath.Join(root, ".env")); !strings.Contains(got, "GEMINI_API_KEY=abc123") {
		t.Fatalf(".env after api key = %q", got)
	}

	postJSONStatus(t, ts.URL+"/api/settings/env", `{"settings":{"GMAIL_EMAIL":"me@example.com","REJECTION_EMAIL_SYNC_DISABLED":true,"GMAIL_APP_PASSWORD":"pw"}}`, http.StatusOK)
	env := readTextFileSafe(filepath.Join(root, ".env"))
	for _, want := range []string{"GMAIL_EMAIL=me@example.com", "REJECTION_EMAIL_SYNC_DISABLED=true", "GMAIL_APP_PASSWORD=pw"} {
		if !strings.Contains(env, want) {
			t.Fatalf(".env missing %s:\n%s", want, env)
		}
	}
	postJSONStatus(t, ts.URL+"/api/settings/env", `{"settings":{"NOPE":"x"}}`, http.StatusBadRequest)
}

func TestCareerAndExperienceWriteRoutesNative(t *testing.T) {
	dataDir := t.TempDir()
	root := t.TempDir()
	t.Chdir(root)
	fake := &fakeRejection{askFn: func(string) (string, error) {
		return "```markdown\n## Acme — Engineer\n- Built things.\n```", nil
	}}
	ts := setupWriteServer(t, dataDir, fake)

	postJSONStatus(t, ts.URL+"/api/setup/career", `{"careerDetail":"career","applicant":"app","voice":"voice"}`, http.StatusOK)
	if got := readTextFileSafe(filepath.Join(dataDir, "career-detail.md")); got != "career" {
		t.Fatalf("career detail = %q", got)
	}
	if got := readTextFileSafe(filepath.Join(root, ".context", "people", "applicant.md")); got != "app" {
		t.Fatalf("applicant = %q", got)
	}

	postJSONStatus(t, ts.URL+"/api/setup/experience", `{"name":"acme.md","content":"experience"}`, http.StatusOK)
	if got := readTextFileSafe(filepath.Join(dataDir, "experience", "acme.md")); got != "experience" {
		t.Fatalf("experience = %q", got)
	}
	postJSONStatus(t, ts.URL+"/api/setup/experience/delete", `{"name":"acme.md"}`, http.StatusOK)
	if _, err := os.Stat(filepath.Join(dataDir, "experience", "acme.md")); !os.IsNotExist(err) {
		t.Fatalf("experience delete err=%v", err)
	}

	resp := postJSONStatus(t, ts.URL+"/api/setup/career/structure", `{"raw":"Acme notes"}`, http.StatusOK)
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["text"] != "## Acme — Engineer\n- Built things." {
		t.Fatalf("structured text = %#v", body["text"])
	}
}

func TestExtractProfileRouteNative(t *testing.T) {
	dataDir := t.TempDir()
	writeTestFile(t, filepath.Join(dataDir, "resume.md"), "resume")
	fake := &fakeRejection{askFn: func(string) (string, error) {
		return `{"titles":["Platform Engineer"],"searchTerms":["platform","go"],"stack":["Go"],"salary":180000,"industry":"tech"}`, nil
	}}
	ts := setupWriteServer(t, dataDir, fake)
	resp := postJSONStatus(t, ts.URL+"/api/setup/extract-profile", `{}`, http.StatusOK)
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["titles"] != "Platform Engineer" || body["searchTerms"] != "platform" {
		t.Fatalf("extract body = %#v", body)
	}
}

func postJSONStatus(t *testing.T, url, body string, want int) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	if resp.StatusCode != want {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("POST %s status=%d want=%d body=%s", url, resp.StatusCode, want, data)
	}
	return resp
}
