package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"job-search-automation/internal/db"
)

func setupReadServer(t *testing.T, dataDir string) *httptest.Server {
	t.Helper()
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	srv, err := New(t.TempDir(), repo, nil, time.Second, 500, dataDir, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestSetupStatusRouteNative(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("GEMINI_API_KEY", "test-key")
	writeTestFile(t, filepath.Join(dataDir, "resume.md"), "resume text")
	writeTestFile(t, filepath.Join(dataDir, "context.md"), "context text")
	writeTestFile(t, filepath.Join(dataDir, "companies.json"), "companies text")
	ts := setupReadServer(t, dataDir)

	resp := get(t, ts.URL+"/api/setup/status", nil)
	defer resp.Body.Close()
	var body setupStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ResumeContent != "resume text" || body.ContextContent != "context text" || body.CompaniesContent != "companies text" || !body.HasKey {
		t.Fatalf("setup status = %+v", body)
	}
	if body.FirstRun {
		t.Fatalf("firstRun should be false once a resume exists: %+v", body)
	}
}

func TestSetupStatusFirstRun(t *testing.T) {
	// Empty profile dir: a brand-new tenant still needs onboarding.
	emptyDir := t.TempDir()
	ts := setupReadServer(t, emptyDir)
	resp := get(t, ts.URL+"/api/setup/status", nil)
	defer resp.Body.Close()
	var body setupStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.FirstRun {
		t.Fatalf("empty tenant should report firstRun=true: %+v", body)
	}

	// The .onboarded marker plus resume clears first-run for completed tenants.
	markedDir := t.TempDir()
	writeTestFile(t, filepath.Join(markedDir, "resume.md"), "resume text")
	writeTestFile(t, filepath.Join(markedDir, ".onboarded"), "2026-06-15T00:00:00Z\n")
	ts2 := setupReadServer(t, markedDir)
	resp2 := get(t, ts2.URL+"/api/setup/status", nil)
	defer resp2.Body.Close()
	var body2 setupStatusResponse
	if err := json.NewDecoder(resp2.Body).Decode(&body2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body2.FirstRun {
		t.Fatalf("onboarded marker should report firstRun=false: %+v", body2)
	}

	// A resume uploaded at wizard step 2 but no companies.json (targets step not
	// yet done) and no marker must STILL report firstRun=true, so a refresh
	// reopens the wizard instead of dropping the user onto an empty dashboard.
	resumeOnlyDir := t.TempDir()
	writeTestFile(t, filepath.Join(resumeOnlyDir, "resume.md"), "resume text")
	ts3 := setupReadServer(t, resumeOnlyDir)
	resp3 := get(t, ts3.URL+"/api/setup/status", nil)
	defer resp3.Body.Close()
	var body3 setupStatusResponse
	if err := json.NewDecoder(resp3.Body).Decode(&body3); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body3.FirstRun {
		t.Fatalf("resume-only (no companies.json, no marker) should report firstRun=true: %+v", body3)
	}

	// Older pre-marker profiles remain onboarded when they have resume.md and a
	// real companies.json, even though context.md is no longer part of the pipeline.
	legacyDir := t.TempDir()
	writeTestFile(t, filepath.Join(legacyDir, "resume.md"), "resume text")
	writeTestFile(t, filepath.Join(legacyDir, "companies.json"), `{"SEARCH_TERMS":["platform"]}`)
	ts4 := setupReadServer(t, legacyDir)
	resp4 := get(t, ts4.URL+"/api/setup/status", nil)
	defer resp4.Body.Close()
	var body4 setupStatusResponse
	if err := json.NewDecoder(resp4.Body).Decode(&body4); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body4.FirstRun {
		t.Fatalf("legacy resume + companies profile should report firstRun=false: %+v", body4)
	}
}

func TestSettingsEnvGetRouteNative(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("GMAIL_EMAIL", "me@example.com")
	t.Setenv("GMAIL_APP_PASSWORD", "secret")
	t.Setenv("REJECTION_EMAIL_SYNC_DISABLED", "true")
	ts := setupReadServer(t, dataDir)

	resp := get(t, ts.URL+"/api/settings/env", nil)
	defer resp.Body.Close()
	var body setupSettingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.OK || len(body.Settings) != len(setupAllowedSettings) {
		t.Fatalf("settings response = %+v", body)
	}
	byKey := map[string]map[string]any{}
	for _, s := range body.Settings {
		byKey[s["key"].(string)] = s
	}
	if byKey["GMAIL_EMAIL"]["value"] != "me@example.com" {
		t.Fatalf("gmail email setting = %+v", byKey["GMAIL_EMAIL"])
	}
	if byKey["GMAIL_APP_PASSWORD"]["set"] != true {
		t.Fatalf("secret setting = %+v", byKey["GMAIL_APP_PASSWORD"])
	}
	if byKey["REJECTION_EMAIL_SYNC_DISABLED"]["checked"] != true {
		t.Fatalf("bool setting = %+v", byKey["REJECTION_EMAIL_SYNC_DISABLED"])
	}
}

func TestCareerGetRouteNative(t *testing.T) {
	dataDir := t.TempDir()
	root := t.TempDir()
	t.Chdir(root)
	writeTestFile(t, filepath.Join(dataDir, "career-detail.md"), "career detail")
	writeTestFile(t, filepath.Join(dataDir, "experience", "zeta.md"), "zeta")
	writeTestFile(t, filepath.Join(dataDir, "experience", "alpha.md"), "alpha")
	writeTestFile(t, filepath.Join(root, ".context", "people", "applicant.md"), "applicant")
	writeTestFile(t, filepath.Join(root, ".context", "people", "voice.md"), "voice")
	ts := setupReadServer(t, dataDir)

	resp := get(t, ts.URL+"/api/setup/career", nil)
	defer resp.Body.Close()
	var body careerGetResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.CareerDetail != "career detail" || body.Applicant != "applicant" || body.Voice != "voice" {
		t.Fatalf("career response = %+v", body)
	}
	if len(body.Experience) != 2 || body.Experience[0].Name != "alpha.md" || body.Experience[1].Name != "zeta.md" {
		t.Fatalf("experience order = %+v", body.Experience)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
