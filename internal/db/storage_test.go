package db

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// writeProfile creates base/storage/users/<uid>/ and writes the given files.
func writeProfile(t *testing.T, base, uid string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(base, "storage", "users", uid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s/%s: %v", uid, name, err)
		}
	}
}

func TestOnboardedTenantDirs(t *testing.T) {
	base := t.TempDir()

	// Onboarded: non-empty resume.md plus the setup marker.
	writeProfile(t, base, "tenant-onboarded", map[string]string{
		"resume.md":  "Jane Doe\nDevOps engineer",
		".onboarded": "2026-06-15T00:00:00Z\n",
	})
	// Legacy pre-marker profile: non-empty resume.md plus real companies.json.
	writeProfile(t, base, "tenant-legacy", map[string]string{
		"resume.md":      "Jane Doe\nDevOps engineer",
		"companies.json": `{"SEARCH_TERMS":["platform"]}`,
	})
	// Resume only: not onboarded (the half-finished-wizard case).
	writeProfile(t, base, "tenant-resume-only", map[string]string{
		"resume.md": "Half way there",
	})
	// Marker without resume, and empty/whitespace resume: not onboarded.
	writeProfile(t, base, "tenant-marker-no-resume", map[string]string{
		".onboarded": "2026-06-15T00:00:00Z\n",
	})
	writeProfile(t, base, "tenant-empty", map[string]string{
		"resume.md":  "   \n",
		".onboarded": "2026-06-15T00:00:00Z\n",
	})
	// Provisioned shell, no profile files: not onboarded.
	writeProfile(t, base, "tenant-shell", map[string]string{})
	// A stray file (not a dir) under users/ must be ignored, not panic.
	if err := os.WriteFile(filepath.Join(base, "storage", "users", "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write stray: %v", err)
	}

	got := OnboardedTenantDirs(base, Postgres)
	sort.Strings(got)
	want := []string{"tenant-legacy", "tenant-onboarded"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("OnboardedTenantDirs = %v, want %v", got, want)
	}

	// Self-host (non-Postgres) never scans the per-tenant root.
	if dirs := OnboardedTenantDirs(base, SQLite); dirs != nil {
		t.Fatalf("OnboardedTenantDirs(SQLite) = %v, want nil", dirs)
	}

	// A missing storage root degrades to nil, not an error/panic.
	if dirs := OnboardedTenantDirs(t.TempDir(), Postgres); dirs != nil {
		t.Fatalf("OnboardedTenantDirs(empty base) = %v, want nil", dirs)
	}
}

// TestRepairStalledProfiles covers the silent onboarding stall: a tenant who
// uploaded a resume and closed the wizard before the search-terms step was
// excluded from every background cron forever, with nothing logged and no error
// anywhere. After the repair they must satisfy tenantProfileComplete and join
// the fan-out.
func TestRepairStalledProfiles(t *testing.T) {
	base := t.TempDir()
	const resume = "Jane Doe\nSenior Platform Engineer | Acme\nBuilt CI/CD pipelines.\n"

	// The stall: resume uploaded, provisioned companies.json with empty lists,
	// no .onboarded marker.
	writeProfile(t, base, "tenant-stalled", map[string]string{
		"resume.md":      resume,
		"companies.json": `{"MAX_AGE_DAYS":20,"SEARCH_TERMS":[],"GREENHOUSE_COMPANIES":[]}`,
	})
	// Already complete: must not be touched.
	writeProfile(t, base, "tenant-done", map[string]string{
		"resume.md":      resume,
		"companies.json": `{"SEARCH_TERMS":["devops"]}`,
		".onboarded":     "2026-06-15T00:00:00Z\n",
	})
	// No resume: nothing to derive from, so it stays stalled rather than being
	// seeded with invented terms.
	writeProfile(t, base, "tenant-no-resume", map[string]string{
		"companies.json": `{"SEARCH_TERMS":[]}`,
	})
	// A resume with no derivable role term is left for the wizard to handle.
	writeProfile(t, base, "tenant-unparseable", map[string]string{
		"resume.md": "...\n",
	})

	got := RepairStalledProfiles(base, Postgres)
	if len(got) != 1 || got[0].UserID != "tenant-stalled" {
		t.Fatalf("RepairStalledProfiles = %+v, want exactly tenant-stalled", got)
	}
	if len(got[0].Terms) == 0 {
		t.Fatal("repaired profile carries no terms")
	}

	// The repaired tenant now passes the completeness check the fan-out uses.
	dir := filepath.Join(base, "storage", "users", "tenant-stalled")
	if !tenantProfileComplete(dir) {
		t.Fatal("repaired profile still judged incomplete")
	}
	// Unrelated keys in companies.json survive the merge.
	raw, err := os.ReadFile(filepath.Join(dir, "companies.json"))
	if err != nil {
		t.Fatalf("read repaired companies.json: %v", err)
	}
	var profile map[string]any
	if err := json.Unmarshal(raw, &profile); err != nil {
		t.Fatalf("repaired companies.json is not valid JSON: %v", err)
	}
	if profile["MAX_AGE_DAYS"] != float64(20) {
		t.Fatalf("MAX_AGE_DAYS = %v, want 20 preserved", profile["MAX_AGE_DAYS"])
	}
	if terms, ok := profile["SEARCH_TERMS"].([]any); !ok || len(terms) == 0 {
		t.Fatalf("SEARCH_TERMS = %v, want non-empty", profile["SEARCH_TERMS"])
	}

	// Idempotent: a second pass finds nothing left to repair.
	if again := RepairStalledProfiles(base, Postgres); len(again) != 0 {
		t.Fatalf("second RepairStalledProfiles = %+v, want none", again)
	}
	// The complete tenant's file was never rewritten.
	doneRaw, err := os.ReadFile(filepath.Join(base, "storage", "users", "tenant-done", "companies.json"))
	if err != nil {
		t.Fatalf("read untouched companies.json: %v", err)
	}
	if string(doneRaw) != `{"SEARCH_TERMS":["devops"]}` {
		t.Fatalf("completed tenant's companies.json was rewritten: %s", doneRaw)
	}

	// Self-host never scans the per-tenant root.
	if r := RepairStalledProfiles(base, SQLite); r != nil {
		t.Fatalf("RepairStalledProfiles(SQLite) = %+v, want nil", r)
	}
}

// TestBackgroundTenantsSelfHost is the self-host invariant: BackgroundTenants
// returns exactly Tenants() ([LocalUser]) and never scans the filesystem, even
// when onboarded-looking tenant dirs exist on disk.
func TestBackgroundTenantsSelfHost(t *testing.T) {
	repo := newTestRepo(t)
	base := t.TempDir()
	writeProfile(t, base, "tenant-onboarded", map[string]string{
		"resume.md":  "resume",
		".onboarded": "2026-06-15T00:00:00Z\n",
	})

	got, err := repo.BackgroundTenants(base)
	if err != nil {
		t.Fatalf("BackgroundTenants: %v", err)
	}
	if len(got) != 1 || got[0] != LocalUser {
		t.Fatalf("BackgroundTenants = %v, want [%q]", got, LocalUser)
	}
}
