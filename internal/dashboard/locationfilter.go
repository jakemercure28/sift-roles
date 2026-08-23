package dashboard

import (
	"job-search-automation/internal/db"
	"job-search-automation/internal/jobfilter"
)

// The metro/location matching rules live in internal/jobfilter so the scoring
// pipeline's pre-score prefilter and this view-time filter share one
// implementation. This file keeps the dashboard-local names as thin wrappers.

// passesPrefs reports whether a job's location/title clears the saved metro
// prefs. Inclusive by design: a chosen metro never hides remote, nationwide, or
// unknown-location roles.
func passesPrefs(location, title string, prefs LocationPrefs) bool {
	return jobfilter.Passes(location, title, prefs)
}

// applyLocationPrefs keeps only the jobs that clear the saved location prefs.
func applyLocationPrefs(jobs []db.ListedJob, prefs LocationPrefs) []db.ListedJob {
	// Fast path: no metro filter and not remote-only means every job passes.
	if prefs.IsDefault() {
		return jobs
	}
	out := jobs[:0:0]
	for _, j := range jobs {
		if passesPrefs(j.Location, j.Title, prefs) {
			out = append(out, j)
		}
	}
	return out
}
