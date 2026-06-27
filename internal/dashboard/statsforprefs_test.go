package dashboard

import (
	"testing"

	"job-search-automation/internal/db"
)

// statsForPrefs scopes the header counts to the active location filter: with no
// metro selected it equals GlobalStats, and with a metro it counts only the jobs
// that pass passesPrefs (metro cities/states plus the always-included remote).
func TestStatsForPrefs(t *testing.T) {
	repo, conn := dataLayerRepo(t)
	srv := &Server{repo: repo}

	seedFullJob(t, conn, db.ListedJob{ID: "s1", Location: "Seattle, WA", Status: "pending"})
	seedFullJob(t, conn, db.ListedJob{ID: "s2", Location: "Bellevue, WA", Status: "pending"})
	seedFullJob(t, conn, db.ListedJob{ID: "n1", Location: "New York, NY", Status: "pending"})
	seedFullJob(t, conn, db.ListedJob{ID: "r1", Location: "Remote", Status: "pending"})
	seedFullJob(t, conn, db.ListedJob{ID: "a1", Location: "Austin, TX", Status: "applied", Stage: "phone_screen"})

	// No location filter: must equal GlobalStats over every row.
	none := LocationPrefs{Metros: []string{}, IncludeUnknown: true}
	got, err := srv.statsForPrefs(none)
	if err != nil {
		t.Fatalf("statsForPrefs(none): %v", err)
	}
	want, _ := repo.GlobalStats()
	if got != want {
		t.Fatalf("unfiltered stats = %+v, want GlobalStats %+v", got, want)
	}

	// Seattle: s1, s2 (metro) and r1 (remote is always included) => 3 pending.
	// The NYC and Austin rows are excluded.
	sea, err := srv.statsForPrefs(LocationPrefs{Metros: []string{"seattle"}, IncludeUnknown: true})
	if err != nil {
		t.Fatalf("statsForPrefs(seattle): %v", err)
	}
	if sea.NotApplied != 3 || sea.Total != 3 {
		t.Fatalf("seattle stats = %+v, want Total=3 NotApplied=3", sea)
	}

	// New York: n1 (metro) and r1 (remote) => 2 pending.
	nyc, err := srv.statsForPrefs(LocationPrefs{Metros: []string{"nyc"}, IncludeUnknown: true})
	if err != nil {
		t.Fatalf("statsForPrefs(nyc): %v", err)
	}
	if nyc.NotApplied != 2 || nyc.Total != 2 {
		t.Fatalf("nyc stats = %+v, want Total=2 NotApplied=2", nyc)
	}
}
