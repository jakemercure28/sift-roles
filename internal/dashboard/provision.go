package dashboard

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

// tenantTemplates holds the structured-but-empty profile files a brand-new hosted
// tenant is seeded with on first login. companies.json is kept byte-equivalent to
// data.example/companies.json so the scraper's onboarding heuristic
// (scraper-service/src/lib/onboarding.ts) still reports the tenant as
// not-yet-onboarded until the setup wizard completes. Resume is intentionally
// absent because it is the only required markdown input.
//
//go:embed templates
var tenantTemplates embed.FS

// provisionTenant lazily seeds a hosted tenant's profile dir the first time it is
// referenced. It is idempotent: only files that do not already exist are written,
// so a returning or half-configured tenant is never clobbered, and after the first
// request it costs a handful of stats.
//
// It deliberately does NOT write resume.md and does NOT write the .onboarded
// marker — only the setup wizard (handleSetupProfile) writes the marker on
// completion. Seeding companies.json equal to the example plus leaving resume.md
// absent keeps the onboarding heuristic's customizedCompanies/hasResume both
// false, so a freshly provisioned tenant is never scraped against demo defaults.
//
// Self-host (where forRequest passes the unchanged root data dir) never calls this:
// the caller guards on dir != base. Passing the base dir here would still be safe,
// but the guard keeps the SQLite path a literal no-op.
func provisionTenant(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// Seed each embedded template that the tenant doesn't already have.
	err := fs.WalkDir(tenantTemplates, "templates", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel("templates", path)
		if err != nil {
			return err
		}
		target := filepath.Join(dir, rel)
		if _, statErr := os.Stat(target); statErr == nil {
			return nil // already present — never clobber a configured tenant
		}
		data, err := tenantTemplates.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return err
	}

	// Structural dirs the dashboard reads from but that carry no seed file.
	// go:embed cannot carry empty directories, so create them explicitly.
	for _, sub := range []string{"experience", filepath.Join(".context", "people")} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return err
		}
	}
	return nil
}
