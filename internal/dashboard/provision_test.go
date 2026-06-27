package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// normalizeWS mirrors the scraper's whitespace-insensitive companies.json compare
// (scraper-service/src/lib/onboarding.ts:sameAsExampleCompanies) so the test
// asserts the same equality the onboarding heuristic relies on.
func normalizeWS(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestProvisionTenantSeedsEmptyProfile(t *testing.T) {
	dir := t.TempDir()
	if err := provisionTenant(dir); err != nil {
		t.Fatalf("provisionTenant: %v", err)
	}

	// companies.json must be present and match the example whitespace-insensitively
	// so the scraper still reads the tenant as not-yet-onboarded.
	got, err := os.ReadFile(filepath.Join(dir, "companies.json"))
	if err != nil {
		t.Fatalf("read seeded companies.json: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("..", "..", "data.example", "companies.json"))
	if err != nil {
		t.Fatalf("read example companies.json: %v", err)
	}
	if normalizeWS(string(got)) != normalizeWS(string(want)) {
		t.Errorf("seeded companies.json differs from data.example:\n got: %s\nwant: %s", got, want)
	}

	// resume.md and the .onboarded marker must be absent — their presence would
	// make the heuristic treat a blank tenant as ready to scrape.
	if _, err := os.Stat(filepath.Join(dir, "resume.md")); !os.IsNotExist(err) {
		t.Errorf("resume.md should not be seeded, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".onboarded")); !os.IsNotExist(err) {
		t.Errorf(".onboarded should not be seeded, stat err = %v", err)
	}

	// Structural dirs the dashboard reads from must exist.
	for _, sub := range []string{"experience", filepath.Join(".context", "people")} {
		info, err := os.Stat(filepath.Join(dir, sub))
		if err != nil || !info.IsDir() {
			t.Errorf("expected dir %q, stat err = %v", sub, err)
		}
	}
}

func TestProvisionTenantIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	// A returning tenant has already customized companies.json; provisioning again
	// must not clobber it.
	custom := []byte(`{"GREENHOUSE_COMPANIES":["acme"]}`)
	if err := os.WriteFile(filepath.Join(dir, "companies.json"), custom, 0o644); err != nil {
		t.Fatalf("seed custom companies.json: %v", err)
	}
	if err := provisionTenant(dir); err != nil {
		t.Fatalf("provisionTenant: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "companies.json"))
	if err != nil {
		t.Fatalf("read companies.json: %v", err)
	}
	if string(got) != string(custom) {
		t.Errorf("provisioning clobbered a configured tenant: got %s", got)
	}
}
