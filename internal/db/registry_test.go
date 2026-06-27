package db

import (
	"context"
	"slices"
	"testing"

	"job-search-automation/internal/discovery"
)

// Upsert then Known round-trips API and Workday boards through the real SQLite
// schema, exercising the dialect rewrite of datetime() and ON CONFLICT.
func TestCompanyRegistryRoundTrip(t *testing.T) {
	repo := newTestRepo(t)
	reg := repo.CompanyRegistry()
	ctx := context.Background()

	boards := []discovery.VerifiedBoard{
		{Platform: "greenhouse", Slug: "warbyparker"},
		{Platform: "lever", Slug: "linear"},
		{Platform: "workday", Workday: &discovery.WorkdayEntry{Sub: "capitalone", WD: 12, Board: "Capital_One", Label: "Capital One"}},
	}
	if err := reg.Upsert(ctx, boards); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	known, err := reg.Known(ctx)
	if err != nil {
		t.Fatalf("Known: %v", err)
	}
	if !slices.Contains(known.Greenhouse, "warbyparker") {
		t.Fatalf("greenhouse = %#v, want warbyparker", known.Greenhouse)
	}
	if !slices.Contains(known.Lever, "linear") {
		t.Fatalf("lever = %#v, want linear", known.Lever)
	}
	if len(known.Workday) != 1 || discovery.WorkdayKey(known.Workday[0]) != "capitalone/Capital_One" {
		t.Fatalf("workday = %+v, want capitalone/Capital_One", known.Workday)
	}
	if known.Workday[0].WD != 12 || known.Workday[0].Label != "Capital One" {
		t.Fatalf("workday entry lost fields: %+v", known.Workday[0])
	}
}

// Re-upserting the same board is idempotent (PRIMARY KEY conflict bumps the
// timestamp rather than duplicating the row).
func TestCompanyRegistryUpsertIdempotent(t *testing.T) {
	repo := newTestRepo(t)
	reg := repo.CompanyRegistry()
	ctx := context.Background()

	board := []discovery.VerifiedBoard{{Platform: "greenhouse", Slug: "warbyparker"}}
	if err := reg.Upsert(ctx, board); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	if err := reg.Upsert(ctx, board); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	known, err := reg.Known(ctx)
	if err != nil {
		t.Fatalf("Known: %v", err)
	}
	if got := slices.Collect(slices.Values(known.Greenhouse)); len(got) != 1 {
		t.Fatalf("greenhouse = %#v, want exactly one row", got)
	}
}

// Known is global: a board harvested by one tenant is visible to another.
func TestCompanyRegistryIsGlobalAcrossTenants(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	if err := repo.ForUser("tenant-a").CompanyRegistry().Upsert(ctx,
		[]discovery.VerifiedBoard{{Platform: "greenhouse", Slug: "warbyparker"}}); err != nil {
		t.Fatalf("Upsert as tenant-a: %v", err)
	}
	known, err := repo.ForUser("tenant-b").CompanyRegistry().Known(ctx)
	if err != nil {
		t.Fatalf("Known as tenant-b: %v", err)
	}
	if !slices.Contains(known.Greenhouse, "warbyparker") {
		t.Fatalf("tenant-b greenhouse = %#v, want warbyparker harvested by tenant-a", known.Greenhouse)
	}
}

// A board last verified beyond the freshness window is excluded from Known.
func TestCompanyRegistryStaleExcluded(t *testing.T) {
	repo := newTestRepo(t)
	ctx := context.Background()

	// Insert a row with a last_verified_at well past the freshness window.
	if _, err := repo.exec(
		`INSERT INTO company_registry (platform, registry_key, slug, last_verified_at)
		 VALUES (?, ?, ?, datetime('now', '-' || ? || ' days'))`,
		"greenhouse", "staleco", "staleco", registryFreshDays+1,
	); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}
	known, err := repo.CompanyRegistry().Known(ctx)
	if err != nil {
		t.Fatalf("Known: %v", err)
	}
	if slices.Contains(known.Greenhouse, "staleco") {
		t.Fatalf("greenhouse = %#v, stale board should be excluded", known.Greenhouse)
	}
}
