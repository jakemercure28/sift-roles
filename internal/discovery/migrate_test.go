package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

// A legacy CommonJS profile is converted to companies.json, the old file is
// removed, and LoadCompanyConfig reads the result.
func TestMigrateCompaniesFile(t *testing.T) {
	dir := t.TempDir()
	legacy := `'use strict';

const MAX_AGE_DAYS = 30;
const SEARCH_TERMS = ['sre', 'platform'];
const GREENHOUSE_COMPANIES = ['stripe'];
const WORKDAY_COMPANIES = [
  { sub: 'jcrew', wd: '5', board: 'JCrewCareers', label: 'J.Crew' },
];

module.exports = { MAX_AGE_DAYS, SEARCH_TERMS, GREENHOUSE_COMPANIES, WORKDAY_COMPANIES };
`
	if err := os.WriteFile(filepath.Join(dir, "companies.js"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	migrated, err := MigrateCompaniesFile(dir)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !migrated {
		t.Fatal("expected migration to run")
	}
	if _, err := os.Stat(filepath.Join(dir, "companies.js")); !os.IsNotExist(err) {
		t.Fatal("legacy companies.js should be removed")
	}

	cfg, err := LoadCompanyConfig(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.SearchTerms) != 2 || cfg.SearchTerms[0] != "sre" {
		t.Fatalf("search terms = %v", cfg.SearchTerms)
	}
	if len(cfg.Greenhouse) != 1 || cfg.Greenhouse[0] != "stripe" {
		t.Fatalf("greenhouse = %v", cfg.Greenhouse)
	}
	if len(cfg.Workday) != 1 || cfg.Workday[0].WD != 5 || cfg.Workday[0].Board != "JCrewCareers" {
		t.Fatalf("workday = %+v", cfg.Workday)
	}

	// Idempotent: a second call is a no-op once companies.json exists.
	again, err := MigrateCompaniesFile(dir)
	if err != nil {
		t.Fatalf("migrate again: %v", err)
	}
	if again {
		t.Fatal("second migration should be a no-op")
	}
}
