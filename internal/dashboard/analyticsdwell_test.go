package dashboard

import "testing"

// TestReachedStageDwellFiltersFlips verifies the dwell-gated reached-stage logic:
// a stage counts only when the job genuinely stood there for at least a day, so
// brief test-flips are excluded while interviews that ended in closed/ghosted (or
// predate recorded history) are still counted. now is pinned to the analytics
// fixture instant (2026-06-08T00:00:00Z), so the cutoff is 2026-06-07 00:00:00.
func TestReachedStageDwellFiltersFlips(t *testing.T) {
	repo, conn := dataLayerRepo(t)

	const jobsSQL = `INSERT INTO jobs (id, title, company, url, status, stage, updated_at, rejected_from_stage, created_at) VALUES
		-- flipped to interview for 10 minutes then back: must NOT count
		('flip', 'Flip', 'FlipCo', 'https://x/flip', 'applied', 'applied', '2026-05-02 00:10:00', NULL, '2026-05-01 00:00:00'),
		-- real interview that lasted 5 days then was closed: must count (the prod bug)
		('real', 'Real', 'RealCo', 'https://x/real', 'closed', 'closed', '2026-05-08 00:00:00', NULL, '2026-05-01 00:00:00'),
		-- left interview with no recorded entry (pre-history), ended closed: must count
		('prehist', 'Pre', 'PreCo', 'https://x/prehist', 'closed', 'closed', '2026-05-10 00:00:00', NULL, '2026-04-01 00:00:00'),
		-- flipped to interview today (after the cutoff), still sitting there: must NOT count
		('today', 'Today', 'TodayCo', 'https://x/today', 'applied', 'interview', '2026-06-07 12:00:00', NULL, '2026-06-01 00:00:00');`
	if _, err := conn.Exec(jobsSQL); err != nil {
		t.Fatalf("seed jobs: %v", err)
	}

	const eventsSQL = `INSERT INTO events (job_id, event_type, from_value, to_value, created_at) VALUES
		('flip', 'stage_change', NULL, 'applied', '2026-05-01 00:00:00'),
		('flip', 'stage_change', 'applied', 'interview', '2026-05-02 00:00:00'),
		('flip', 'stage_change', 'interview', 'applied', '2026-05-02 00:10:00'),
		('real', 'stage_change', NULL, 'applied', '2026-05-01 00:00:00'),
		('real', 'stage_change', 'applied', 'interview', '2026-05-03 00:00:00'),
		('real', 'stage_change', 'interview', 'closed', '2026-05-08 00:00:00'),
		('prehist', 'stage_change', 'interview', 'closed', '2026-05-10 00:00:00'),
		('today', 'stage_change', 'applied', 'interview', '2026-06-07 12:00:00');`
	if _, err := conn.Exec(eventsSQL); err != nil {
		t.Fatalf("seed events: %v", err)
	}

	metrics, err := computeAnalyticsMetrics(repo, fixedAnalyticsNow)
	if err != nil {
		t.Fatalf("compute: %v", err)
	}

	// real + prehist count; flip and today do not.
	if metrics.Funnel.Interview != 2 || metrics.Health.Interviews != 2 {
		t.Fatalf("interview funnel/health = %d/%d, want 2/2 (real + prehist; flip and same-day flip excluded)",
			metrics.Funnel.Interview, metrics.Health.Interviews)
	}
}
