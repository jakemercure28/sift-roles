package discovery

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRegistry struct {
	known    Suggested
	mu       sync.Mutex
	upserted []VerifiedBoard
}

func (f *fakeRegistry) Known(context.Context) (Suggested, error) { return f.known, nil }

func (f *fakeRegistry) Upsert(_ context.Context, boards []VerifiedBoard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserted = append(f.upserted, boards...)
	return nil
}

// countingClient returns 200 for any greenhouse board whose path contains
// wantPath, 404 otherwise, and records how many requests it served.
func countingClient(wantPath string, count *int, mu *sync.Mutex) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		*count++
		mu.Unlock()
		status := http.StatusNotFound
		if r.URL.Host == "boards-api.greenhouse.io" && strings.Contains(r.URL.Path, wantPath) {
			status = http.StatusOK
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    r,
		}, nil
	})}
}

// runDiscoveryForRegistryTest runs a single non-bootstrap pass proposing one
// greenhouse company, against the given registry and HTTP client.
func runDiscoveryForRegistryTest(t *testing.T, reg Registry, client *http.Client) error {
	t.Helper()
	dir := t.TempDir()
	writeCompaniesJS(t, dir)
	stale := "2026-05-07T00:00:00.000Z"
	if err := SaveSuggested(dir, seededSuggested(BootstrapThreshold, stale)); err != nil {
		t.Fatalf("SaveSuggested: %v", err)
	}
	gemini := &fakeGemini{raw: `[{"name":"Warby Parker","platform":"Greenhouse","slug":"Warby Parker","rationale":"fit"}]`}
	_, err := Run(context.Background(), Config{
		DataDir:        dir,
		TTLHours:       DefaultTTLHours,
		CandidateCount: DefaultCandidateCount,
		Gemini:         gemini,
		HTTPClient:     client,
		Registry:       reg,
		Now:            func() time.Time { return time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC) },
	})
	return err
}

// A freshly probed board is harvested into the registry for other tenants.
func TestRunHarvestsVerifiedBoards(t *testing.T) {
	reg := &fakeRegistry{known: emptySuggested()}
	var calls int
	var mu sync.Mutex
	if err := runDiscoveryForRegistryTest(t, reg, countingClient("/warbyparker/", &calls, &mu)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(reg.upserted) != 1 {
		t.Fatalf("upserted = %+v, want exactly one board", reg.upserted)
	}
	if got := reg.upserted[0]; got.Platform != "greenhouse" || got.Slug != "warbyparker" {
		t.Fatalf("harvested board = %+v, want greenhouse/warbyparker", got)
	}
	if calls == 0 {
		t.Fatal("expected an HTTP probe for a board not in the registry")
	}
}

// A board already verified in the registry skips the HTTP probe, is added to the
// tenant's suggested list, and is not re-harvested.
func TestRunUsesRegistryCacheAndSkipsProbe(t *testing.T) {
	reg := &fakeRegistry{known: Suggested{
		Greenhouse: []string{"warbyparker"},
		Ashby:      []string{},
		Lever:      []string{},
		Workday:    []WorkdayEntry{},
	}}
	var calls int
	var mu sync.Mutex
	if err := runDiscoveryForRegistryTest(t, reg, countingClient("/warbyparker/", &calls, &mu)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 0 {
		t.Fatalf("made %d HTTP probes, want 0 (registry cache hit)", calls)
	}
	if len(reg.upserted) != 0 {
		t.Fatalf("upserted = %+v, want none (cache hits are not re-harvested)", reg.upserted)
	}
}

// A board added to the tenant's suggested list via a registry cache hit is
// persisted, so the cache still feeds per-tenant discovery results.
func TestRegistryCacheHitStillAddsToSuggested(t *testing.T) {
	dir := t.TempDir()
	writeCompaniesJS(t, dir)
	stale := "2026-05-07T00:00:00.000Z"
	if err := SaveSuggested(dir, seededSuggested(BootstrapThreshold, stale)); err != nil {
		t.Fatalf("SaveSuggested: %v", err)
	}
	reg := &fakeRegistry{known: Suggested{Greenhouse: []string{"warbyparker"}}}
	var calls int
	var mu sync.Mutex
	gemini := &fakeGemini{raw: `[{"name":"Warby Parker","platform":"Greenhouse","slug":"Warby Parker"}]`}
	report, err := Run(context.Background(), Config{
		DataDir:        dir,
		TTLHours:       DefaultTTLHours,
		CandidateCount: DefaultCandidateCount,
		Gemini:         gemini,
		HTTPClient:     countingClient("/warbyparker/", &calls, &mu),
		Registry:       reg,
		Now:            func() time.Time { return time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Added != 1 {
		t.Fatalf("report.Added = %d, want 1 from the cache hit", report.Added)
	}
	saved := LoadSuggested(dir)
	if !slices.Contains(saved.Greenhouse, "warbyparker") {
		t.Fatalf("greenhouse = %#v, want warbyparker added from cache", saved.Greenhouse)
	}
}
