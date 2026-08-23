package db

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"job-search-automation/internal/searchterms"
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
		SearchTerms     []string `json:"SEARCH_TERMS"`
		Greenhouse      []string `json:"GREENHOUSE_COMPANIES"`
		Lever           []string `json:"LEVER_COMPANIES"`
		Workable        []string `json:"WORKABLE_COMPANIES"`
		Ashby           []string `json:"ASHBY_COMPANIES"`
		Workday         []any    `json:"WORKDAY_COMPANIES"`
		Rippling        []string `json:"RIPPLING_COMPANIES"`
		Gem             []string `json:"GEM_COMPANIES"`
		SmartRecruiters []string `json:"SMARTRECRUITERS_COMPANIES"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return true
	}
	return len(cfg.SearchTerms) > 0 || len(cfg.Greenhouse) > 0 || len(cfg.Lever) > 0 ||
		len(cfg.Workable) > 0 || len(cfg.Ashby) > 0 || len(cfg.Workday) > 0 ||
		len(cfg.Rippling) > 0 || len(cfg.Gem) > 0 || len(cfg.SmartRecruiters) > 0
}

// ProfileRepair records one stalled tenant profile that RepairStalledProfiles
// completed, for the caller to log.
type ProfileRepair struct {
	UserID string
	Terms  []string
}

// RepairStalledProfiles completes tenant profiles that are stranded halfway
// through setup: a real resume.md on disk, but no search terms and no
// .onboarded marker.
//
// The setup wizard writes resume.md at the upload step and search terms at the
// final profile step. Anyone who closes the tab in between leaves a profile that
// tenantProfileComplete rejects, so the tenant is excluded from every background
// cron forever and their dashboard stays empty with no error anywhere. It is
// silent: the tenant looks like a normal signup in auth.users and simply never
// receives a job.
//
// The repair derives the terms from the resume with searchterms.FromResume, the
// same function the wizard itself falls back to when the terms box is left
// empty, so the result is exactly what finishing the wizard would have written.
// No seeds are invented: a resume that yields no terms is left alone for the
// wizard to handle, and a tenant with no resume is never touched. Only
// SEARCH_TERMS is set, so any company lists already on disk survive.
//
// Self-host SQLite has no per-tenant storage root and is a no-op.
func RepairStalledProfiles(base string, dt DBType) []ProfileRepair {
	if dt != Postgres {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(base, "storage", "users"))
	if err != nil {
		return nil
	}
	var repaired []ProfileRepair
	for _, e := range entries {
		uid := e.Name()
		if !e.IsDir() || uid == "" || uid == LocalUser {
			continue
		}
		dir := filepath.Join(base, "storage", "users", uid)
		if tenantProfileComplete(dir) {
			continue
		}
		terms := repairProfileTerms(dir)
		if len(terms) == 0 {
			continue
		}
		repaired = append(repaired, ProfileRepair{UserID: uid, Terms: terms})
	}
	return repaired
}

// repairProfileTerms writes resume-derived SEARCH_TERMS into a stalled tenant's
// companies.json and returns what it wrote, or nil when the profile is not a
// repair candidate (no resume, no derivable terms) or the write fails. Failures
// are silent by design: the next cycle retries, and a tenant that cannot be
// repaired is exactly as stuck as before, never worse.
func repairProfileTerms(dir string) []string {
	resume, err := os.ReadFile(filepath.Join(dir, "resume.md"))
	if err != nil || strings.TrimSpace(string(resume)) == "" {
		return nil
	}
	terms := searchterms.FromResume(string(resume))
	if len(terms) == 0 {
		return nil
	}
	// Merge into the existing file rather than rewriting it, so keys this code
	// does not know about (and any companies discovery already found) survive.
	profile := map[string]any{}
	if raw, err := os.ReadFile(filepath.Join(dir, "companies.json")); err == nil {
		if err := json.Unmarshal(raw, &profile); err != nil {
			return nil
		}
	}
	profile["SEARCH_TERMS"] = terms
	out, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return nil
	}
	if err := os.WriteFile(filepath.Join(dir, "companies.json"), append(out, '\n'), 0o644); err != nil {
		return nil
	}
	return terms
}
