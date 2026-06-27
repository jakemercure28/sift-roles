package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildAnalyticsAuditParity(t *testing.T) {
	data := loadAnalyticsData(t) // seeds analytics-seed.sql, computes at fixed now
	audit := buildAnalyticsAudit(data.Metrics)
	got, err := marshalNoHTMLEscape(audit)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertGolden(t, "analytics-audit.json.golden", string(got))
}

func TestAnalyticsAuditRouteNative(t *testing.T) {
	ts, _, conn := fragmentServer(t)
	seed, err := os.ReadFile(filepath.Join("testdata", "analytics-seed.sql"))
	if err != nil {
		t.Fatalf("read seed: %v", err)
	}
	if _, err := conn.Exec(string(seed)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resp := get(t, ts.URL+"/api/analytics/audit", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	var body struct {
		Warnings    []string       `json:"warnings"`
		Definitions map[string]any `json:"definitions"`
		Health      map[string]any `json:"health"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Warnings) != 5 {
		t.Fatalf("warnings = %v, want 5", body.Warnings)
	}
	if body.Definitions["appliedCohort"] == nil {
		t.Fatal("definitions missing appliedCohort")
	}
	if body.Health["applied"].(float64) != 9 {
		t.Fatalf("health.applied = %v, want 9", body.Health["applied"])
	}
}
