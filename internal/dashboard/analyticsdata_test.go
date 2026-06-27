package dashboard

import (
	"os"
	"path/filepath"
	"testing"
)

// fixedAnalyticsNow pins "now" for the analytics golden fixture (2026-06-08T00:00:00Z).
const fixedAnalyticsNow int64 = 1780876800000

// TestComputeAnalyticsMetricsParity seeds the shared analytics fixtures and asserts
// the Go metrics JSON matches the Node golden byte-for-byte.
func TestComputeAnalyticsMetricsParity(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	seed, err := os.ReadFile(filepath.Join("testdata", "analytics-seed.sql"))
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	if _, err := conn.Exec(string(seed)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	metrics, err := computeAnalyticsMetrics(repo, fixedAnalyticsNow)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	got, err := marshalNoHTMLEscape(metrics)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertGolden(t, "analytics-metrics.json.golden", string(got))
}

func TestComputeAnalyticsMetricsUsesHistoricalPipelineEvidence(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	seed, err := os.ReadFile(filepath.Join("testdata", "analytics-seed.sql"))
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	if _, err := conn.Exec(string(seed)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	metrics, err := computeAnalyticsMetrics(repo, fixedAnalyticsNow)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	if metrics.Health.Applied != 9 {
		t.Fatalf("applied = %d, want 9 historical applications", metrics.Health.Applied)
	}
	if metrics.Health.Interviews != 4 || metrics.Funnel.Interview != 4 {
		t.Fatalf("interviews health/funnel = %d/%d, want 4/4", metrics.Health.Interviews, metrics.Funnel.Interview)
	}
	if metrics.Health.Active != 3 {
		t.Fatalf("active = %d, want 3 current open applications", metrics.Health.Active)
	}
	if metrics.Actions.HighScoreUnapplied.Count != 1 {
		t.Fatalf("high-score unapplied = %d, want only unrecovered pending role", metrics.Actions.HighScoreUnapplied.Count)
	}
	if metrics.Reconciliation.AdvancedWithoutAppliedMarker != 3 {
		t.Fatalf("advancedWithoutAppliedMarker = %d, want 3", metrics.Reconciliation.AdvancedWithoutAppliedMarker)
	}
}

func TestComputeAnalyticsPageMetricsMatchesRenderedFullMetrics(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	seed, err := os.ReadFile(filepath.Join("testdata", "analytics-seed.sql"))
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	if _, err := conn.Exec(string(seed)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	full, err := computeAnalyticsMetrics(repo, fixedAnalyticsNow)
	if err != nil {
		t.Fatalf("full metrics: %v", err)
	}
	page, err := computeAnalyticsPageMetrics(repo, fixedAnalyticsNow)
	if err != nil {
		t.Fatalf("page metrics: %v", err)
	}

	if page.Health.Active != full.Health.Active ||
		page.Health.Applied != full.Health.Applied ||
		page.Health.Responded != full.Health.Responded ||
		page.Health.Interviews != full.Health.Interviews ||
		page.Health.Rejected != full.Health.Rejected ||
		page.Health.Stale != full.Health.Stale ||
		!ratesEqual(page.Health.ResponseRate, full.Health.ResponseRate) ||
		!ratesEqual(page.Health.InterviewRate, full.Health.InterviewRate) {
		t.Fatalf("page health = %+v, want %+v", page.Health, full.Health)
	}
	if page.Funnel != full.Funnel {
		t.Fatalf("page funnel = %+v, want %+v", page.Funnel, full.Funnel)
	}
}

func ratesEqual(a, b Rate) bool {
	if a.N != b.N || a.D != b.D {
		return false
	}
	if a.Pct == nil || b.Pct == nil {
		return a.Pct == nil && b.Pct == nil
	}
	return *a.Pct == *b.Pct
}
