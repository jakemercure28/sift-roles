package discovery

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
)

// fakeRegistry is declared in registry_test.go; these tests reuse it.

// harvestDir writes a companies.json with the given search terms and an empty
// suggested list, returning the temp data dir.
func harvestDir(t *testing.T, terms []string) string {
	t.Helper()
	dir := t.TempDir()
	termJSON, _ := json.Marshal(terms)
	raw := `{"SEARCH_TERMS": ` + string(termJSON) + `,
	  "GREENHOUSE_COMPANIES": [], "LEVER_COMPANIES": [], "ASHBY_COMPANIES": [],
	  "WORKABLE_COMPANIES": [], "RIPPLING_COMPANIES": [], "WORKDAY_COMPANIES": []}`
	if err := os.WriteFile(filepath.Join(dir, "companies.json"), []byte(raw), 0o644); err != nil {
		t.Fatalf("write companies.json: %v", err)
	}
	return dir
}

func harvestConfig(dir string, reg Registry, lister PostingsLister) Config {
	return Config{DataDir: dir, Registry: reg, Postings: lister}
}

func TestHarvestAdoptsMatchingBoardsOnly(t *testing.T) {
	dir := harvestDir(t, []string{"merchant", "buyer"})
	reg := &fakeRegistry{known: Suggested{
		Lever:      []string{"brooksrunning"},
		Greenhouse: []string{"randomtech"},
	}}
	lister := func(_ context.Context, platform, slug string) ([]string, bool, error) {
		switch slug {
		case "brooksrunning":
			return []string{"Associate Merchant, Footwear", "Warehouse Lead"}, true, nil
		case "randomtech":
			return []string{"Staff Software Engineer", "SRE"}, true, nil
		}
		return nil, false, nil
	}
	rep, err := Harvest(context.Background(), harvestConfig(dir, reg, lister), HarvestLimits{})
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	if rep.Adopted != 1 || !slices.Contains(rep.Names, "lever:brooksrunning") {
		t.Fatalf("expected only brooksrunning adopted, got %+v", rep)
	}
	got := LoadSuggested(dir)
	if !slices.Contains(got.Lever, "brooksrunning") {
		t.Fatalf("brooksrunning not persisted to lever list: %+v", got.Lever)
	}
	if slices.Contains(got.Greenhouse, "randomtech") {
		t.Fatalf("non-matching randomtech should not be adopted: %+v", got.Greenhouse)
	}
	// updatedAt must stay nil: harvest must not disturb the Gemini discovery TTL.
	if got.UpdatedAt != nil {
		t.Fatalf("harvest must not set updatedAt, got %v", *got.UpdatedAt)
	}
}

// TestHarvestAdoptsSmartRecruiters: a SmartRecruiters registry board with a
// matching posting title is adopted into the tenant's suggested list, the same
// relevance-gated distribution path greenhouse/ashby/lever use.
func TestHarvestAdoptsSmartRecruiters(t *testing.T) {
	dir := harvestDir(t, []string{"platform engineer"})
	reg := &fakeRegistry{known: Suggested{
		SmartRecruiters: []string{"Freshworks", "SomeRetailer"},
	}}
	lister := func(_ context.Context, platform, slug string) ([]string, bool, error) {
		if platform != "smartrecruiters" {
			return nil, false, nil
		}
		switch slug {
		case "Freshworks":
			return []string{"Senior Platform Engineer", "Recruiter"}, true, nil
		case "SomeRetailer":
			return []string{"Store Associate", "Cashier"}, true, nil
		}
		return nil, false, nil
	}
	rep, err := Harvest(context.Background(), harvestConfig(dir, reg, lister), HarvestLimits{})
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	if rep.Adopted != 1 || !slices.Contains(rep.Names, "smartrecruiters:Freshworks") {
		t.Fatalf("expected only Freshworks adopted, got %+v", rep)
	}
	got := LoadSuggested(dir)
	if !slices.Contains(got.SmartRecruiters, "Freshworks") {
		t.Fatalf("Freshworks not persisted to smartrecruiters list: %+v", got.SmartRecruiters)
	}
	if slices.Contains(got.SmartRecruiters, "SomeRetailer") {
		t.Fatalf("non-matching SomeRetailer should not be adopted: %+v", got.SmartRecruiters)
	}
}

func TestHarvestSkipsWithoutSearchTerms(t *testing.T) {
	dir := harvestDir(t, nil)
	reg := &fakeRegistry{known: Suggested{Lever: []string{"brooksrunning"}}}
	var probes int32
	lister := func(context.Context, string, string) ([]string, bool, error) {
		atomic.AddInt32(&probes, 1)
		return []string{"Merchant"}, true, nil
	}
	rep, err := Harvest(context.Background(), harvestConfig(dir, reg, lister), HarvestLimits{})
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	if !rep.Skipped || rep.Reason != "no_search_terms" {
		t.Fatalf("expected skip on no_search_terms, got %+v", rep)
	}
	if probes != 0 {
		t.Fatalf("no boards should be probed without search terms, got %d", probes)
	}
}

func TestHarvestDoesNotReprobeEvaluated(t *testing.T) {
	dir := harvestDir(t, []string{"merchant"})
	reg := &fakeRegistry{known: Suggested{Greenhouse: []string{"norolesco"}}}
	var probes int32
	lister := func(context.Context, string, string) ([]string, bool, error) {
		atomic.AddInt32(&probes, 1)
		return []string{"Software Engineer"}, true, nil // no merchant role -> not adopted
	}
	cfg := harvestConfig(dir, reg, lister)
	if _, err := Harvest(context.Background(), cfg, HarvestLimits{}); err != nil {
		t.Fatalf("first Harvest: %v", err)
	}
	if probes != 1 {
		t.Fatalf("first pass should probe once, got %d", probes)
	}
	// Second pass: the non-matching board was recorded as evaluated, so it is not
	// re-fetched.
	rep, err := Harvest(context.Background(), cfg, HarvestLimits{})
	if err != nil {
		t.Fatalf("second Harvest: %v", err)
	}
	if probes != 1 {
		t.Fatalf("evaluated board must not be re-probed, got %d probes", probes)
	}
	if !rep.Skipped || rep.Reason != "nothing_new" {
		t.Fatalf("second pass should have nothing new, got %+v", rep)
	}
}

func TestHarvestUnreachableBoardStaysEvaluable(t *testing.T) {
	dir := harvestDir(t, []string{"merchant"})
	reg := &fakeRegistry{known: Suggested{Lever: []string{"flaky"}}}
	var calls int32
	lister := func(context.Context, string, string) ([]string, bool, error) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			return nil, false, nil // unreachable on first pass
		}
		return []string{"Senior Merchant"}, true, nil
	}
	cfg := harvestConfig(dir, reg, lister)
	if rep, err := Harvest(context.Background(), cfg, HarvestLimits{}); err != nil || rep.Adopted != 0 {
		t.Fatalf("first pass: rep=%+v err=%v", rep, err)
	}
	// A board that failed to load (ok=false) is not burned; the next pass retries
	// and, now reachable with a matching role, adopts it.
	rep, err := Harvest(context.Background(), cfg, HarvestLimits{})
	if err != nil {
		t.Fatalf("second Harvest: %v", err)
	}
	if rep.Adopted != 1 {
		t.Fatalf("retried board with matching role should be adopted, got %+v", rep)
	}
}

func TestHarvestRespectsMaxAdopt(t *testing.T) {
	dir := harvestDir(t, []string{"merchant"})
	reg := &fakeRegistry{known: Suggested{Lever: []string{"a", "b", "c"}}}
	lister := func(context.Context, string, string) ([]string, bool, error) {
		return []string{"Merchant"}, true, nil
	}
	rep, err := Harvest(context.Background(), harvestConfig(dir, reg, lister), HarvestLimits{MaxAdopt: 2})
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	if rep.Adopted != 2 {
		t.Fatalf("MaxAdopt=2 should cap adoption at 2, got %d", rep.Adopted)
	}
}

func TestHarvestSkipsAlreadyTracked(t *testing.T) {
	dir := harvestDir(t, []string{"merchant"})
	// Pre-track brooksrunning in the suggested list.
	if err := SaveSuggested(dir, Suggested{Lever: []string{"brooksrunning"}}); err != nil {
		t.Fatalf("seed suggested: %v", err)
	}
	reg := &fakeRegistry{known: Suggested{Lever: []string{"brooksrunning"}}}
	var probes int32
	lister := func(context.Context, string, string) ([]string, bool, error) {
		atomic.AddInt32(&probes, 1)
		return []string{"Merchant"}, true, nil
	}
	rep, err := Harvest(context.Background(), harvestConfig(dir, reg, lister), HarvestLimits{})
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	if probes != 0 || rep.Adopted != 0 {
		t.Fatalf("already-tracked board must be skipped, probes=%d rep=%+v", probes, rep)
	}
}

func TestHarvestRespectsMaxProbe(t *testing.T) {
	dir := harvestDir(t, []string{"merchant"})
	reg := &fakeRegistry{known: Suggested{Lever: []string{"a", "b", "c", "d"}}}
	var probes atomic.Int32
	lister := func(context.Context, string, string) ([]string, bool, error) {
		probes.Add(1)
		return []string{"Merchant"}, true, nil
	}
	rep, err := Harvest(context.Background(), harvestConfig(dir, reg, lister), HarvestLimits{MaxProbe: 2, MaxAdopt: 10})
	if err != nil {
		t.Fatalf("Harvest: %v", err)
	}
	if rep.Probed != 2 || probes.Load() != 2 {
		t.Fatalf("MaxProbe=2 should cap probes at 2, got probed=%d calls=%d", rep.Probed, probes.Load())
	}
}
