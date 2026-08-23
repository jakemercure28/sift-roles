package jobfilter

import "testing"

// Guard against "Walthamstow, UK" matching the MA state code via raw substring.
func TestMatchesStateWordBoundary(t *testing.T) {
	if matchesState("walthamstow, uk", []string{"ma"}) {
		t.Error("substring 'ma' in 'walthamstow' should not match state code ma")
	}
	if !matchesState("boston, ma", []string{"ma"}) {
		t.Error("'boston, ma' should match state code ma")
	}
}

// Evaluate must agree with Passes and attribute failures to the right reason,
// since the prefilter archives with archive_reason derived from it.
func TestEvaluateReasons(t *testing.T) {
	seattle := Prefs{Metros: []string{"seattle"}, IncludeUnknown: true}
	seattleNoUnknown := Prefs{Metros: []string{"seattle"}, IncludeUnknown: false}
	remoteOnly := Prefs{IncludeUnknown: true, RemoteOnly: true}

	tests := []struct {
		name       string
		location   string
		title      string
		prefs      Prefs
		wantOK     bool
		wantReason string
	}{
		{"default prefs pass everything", "London, UK", "", Default(), true, ""},
		{"metro match passes", "Seattle, WA", "", seattle, true, ""},
		{"metro miss is location", "New York, NY", "", seattle, false, ReasonLocation},
		{"foreign remote under metro is non_us", "Remote, United Kingdom", "", seattle, false, ReasonNonUS},
		{"unknown excluded is location", "", "", seattleNoUnknown, false, ReasonLocation},
		{"unknown included passes", "", "", seattle, true, ""},
		{"remoteOnly onsite is location", "Seattle, WA", "", remoteOnly, false, ReasonLocation},
		{"remoteOnly foreign remote is non_us", "Remote, Germany", "", remoteOnly, false, ReasonNonUS},
		{"remoteOnly US remote passes", "Remote, US", "", remoteOnly, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, reason := Evaluate(tt.location, tt.title, tt.prefs)
			if ok != tt.wantOK || reason != tt.wantReason {
				t.Errorf("Evaluate(%q, %q, %+v) = (%v, %q), want (%v, %q)",
					tt.location, tt.title, tt.prefs, ok, reason, tt.wantOK, tt.wantReason)
			}
			if got := Passes(tt.location, tt.title, tt.prefs); got != ok {
				t.Errorf("Passes disagrees with Evaluate for %q", tt.name)
			}
		})
	}
}
