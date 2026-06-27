package dashboard

import (
	"os"
	"path/filepath"
	"testing"
)

func loadAnalyticsData(t *testing.T) AnalyticsData {
	t.Helper()
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
		t.Fatalf("metrics: %v", err)
	}
	events, err := repo.RecentEvents()
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	rej, err := repo.RejectionInsights()
	if err != nil {
		t.Fatalf("rejection: %v", err)
	}
	return AnalyticsData{Metrics: metrics, RecentEvents: events, RejectionInsights: rej}
}

func TestRenderAnalyticsParity(t *testing.T) {
	data := loadAnalyticsData(t)
	assertGolden(t, "analytics.html.golden", renderAnalytics(data))
}

func TestRenderActivityLogParity(t *testing.T) {
	data := loadAnalyticsData(t)
	assertGolden(t, "activity-log.html.golden", renderActivityLog(data.RecentEvents))
}
