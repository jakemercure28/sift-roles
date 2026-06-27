package db

import (
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
