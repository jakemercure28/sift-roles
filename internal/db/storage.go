package db

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// TenantDataDir returns the per-tenant profile/storage directory for a user.
// SQLite self-host and the LocalUser tenant get the base dir unchanged, keeping
// the zero-config self-host path byte-for-byte identical; hosted Postgres nests
// each user's files under base/storage/users/{user_id}/ so tenants stay
// sandboxed.
//
// It lives in the db package (the lowest layer that already knows DBType and
// LocalUser) so both the dashboard request scope (internal/dashboard) and the
// one-shot migrator (cmd/migrate-local) resolve tenant paths through one
// definition instead of duplicating the mapping.
func TenantDataDir(base string, dt DBType, userID string) string {
	if dt != Postgres || userID == "" || userID == LocalUser {
		return base
	}
	return filepath.Join(base, "storage", "users", userID)
}

// OnboardedTenantDirs scans the hosted per-tenant storage root (base/storage/users)
// and returns the user_ids whose profile has completed setup, judged the same way
// the dashboard's tenantOnboarded fallback does: a non-empty resume.md plus the
// .onboarded marker or a real companies.json. The background crons union this with the jobs-derived
// Tenants() set (see Repository.BackgroundTenants) so a tenant that finished the
// wizard but has not yet produced any job rows is still serviced, instead of being
// stranded out of the fan-out forever.
//
// Self-host SQLite has no per-tenant storage root and returns nil, leaving that
// path unchanged. Filesystem errors (missing root, unreadable dir) degrade to nil:
// the jobs-derived set still drives the fan-out, so a transient FS problem never
// drops established tenants.
func OnboardedTenantDirs(base string, dt DBType) []string {
	if dt != Postgres {
		return nil
	}
	root := filepath.Join(base, "storage", "users")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var uids []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		uid := e.Name()
		if uid == "" || uid == LocalUser {
			continue
		}
		dir := filepath.Join(root, uid)
		if tenantProfileComplete(dir) {
			uids = append(uids, uid)
		}
	}
	return uids
}

func tenantProfileComplete(dir string) bool {
	if !nonEmptyProfileFile(filepath.Join(dir, "resume.md")) {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, ".onboarded")); err == nil {
		return true
	}
	return companiesFileHasSignal(filepath.Join(dir, "companies.json"))
}

func nonEmptyProfileFile(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) != ""
}

func companiesFileHasSignal(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(data)) == "" {
		return false
	}
	var cfg struct {
		SearchTerms []string `json:"SEARCH_TERMS"`
		Greenhouse  []string `json:"GREENHOUSE_COMPANIES"`
		Lever       []string `json:"LEVER_COMPANIES"`
		Workable    []string `json:"WORKABLE_COMPANIES"`
		Ashby       []string `json:"ASHBY_COMPANIES"`
		Workday     []any    `json:"WORKDAY_COMPANIES"`
		Rippling    []string `json:"RIPPLING_COMPANIES"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return true
	}
	return len(cfg.SearchTerms) > 0 || len(cfg.Greenhouse) > 0 || len(cfg.Lever) > 0 ||
		len(cfg.Workable) > 0 || len(cfg.Ashby) > 0 || len(cfg.Workday) > 0 ||
		len(cfg.Rippling) > 0
}
