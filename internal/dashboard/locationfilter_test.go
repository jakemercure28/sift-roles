package dashboard

import "testing"

func prefs(metros []string, includeUnknown, remoteOnly bool) LocationPrefs {
	return LocationPrefs{Metros: metros, IncludeUnknown: includeUnknown, RemoteOnly: remoteOnly}
}

func TestPassesPrefs(t *testing.T) {
	seattle := []string{"seattle"}
	tests := []struct {
		name     string
		location string
		title    string
		prefs    LocationPrefs
		want     bool
	}{
		{"city match", "Seattle, WA", "Engineer", prefs(seattle, true, false), true},
		{"state match", "Bellevue, WA", "", prefs(seattle, true, false), true},
		{"other metro excluded", "Austin, TX", "", prefs(seattle, true, false), false},
		{"remote included when metro set", "Remote (US)", "", prefs(seattle, true, false), true},
		{"remote in title included", "New York, NY", "Senior SRE (Remote or Seattle)", prefs(seattle, true, false), true},
		{"nationwide included", "United States", "", prefs(seattle, true, false), true},
		{"nationwide with city not auto-included", "Austin, United States", "", prefs(seattle, true, false), false},
		{"unknown kept when includeUnknown", "", "Engineer", prefs(seattle, true, false), true},
		{"unknown dropped when not includeUnknown", "", "Engineer", prefs(seattle, false, false), false},
		{"no metro filter passes all", "Austin, TX", "", prefs(nil, true, false), true},
		{"remoteOnly keeps remote", "Remote", "", prefs(nil, true, true), true},
		{"remoteOnly drops onsite", "Seattle, WA", "", prefs(seattle, true, true), false},
		{"remoteOnly drops onsite even with no metro", "Austin, TX", "", prefs(nil, true, true), false},

		// International-remote must not leak past a US metro filter just because the
		// string contains "remote". US-eligible remote roles still pass.
		{"foreign remote UK excluded", "Remote, United Kingdom", "", prefs(seattle, true, false), false},
		{"foreign remote Sweden excluded", "Remote, Sweden", "", prefs(seattle, true, false), false},
		{"foreign remote Ireland excluded", "Remote, Republic of Ireland", "", prefs(seattle, true, false), false},
		{"foreign remote Germany excluded", "Remote, Germany", "", prefs(seattle, true, false), false},
		{"foreign remote Canada excluded", "Remote, Canada", "", prefs(seattle, true, false), false},
		{"EU remote excluded", "Remote, from EU", "", prefs(seattle, true, false), false},
		{"bare remote still included", "Remote", "", prefs(seattle, true, false), true},
		{"US remote included", "Remote, US", "", prefs(seattle, true, false), true},
		{"US or Canada remote included", "Remote, US or Canada", "", prefs(seattle, true, false), true},
		{"remoteOnly drops foreign remote", "Remote, United Kingdom", "", prefs(nil, true, true), false},
		{"remoteOnly keeps US remote", "Remote, US", "", prefs(nil, true, true), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := passesPrefs(tt.location, tt.title, tt.prefs); got != tt.want {
				t.Errorf("passesPrefs(%q, %q, %+v) = %v, want %v", tt.location, tt.title, tt.prefs, got, tt.want)
			}
		})
	}
}

// Guard against "Walthamstow, UK" matching the MA state code via raw substring.
func TestMatchesStateWordBoundary(t *testing.T) {
	if matchesState("walthamstow, uk", []string{"ma"}) {
		t.Error("substring 'ma' in 'walthamstow' should not match state code ma")
	}
	if !matchesState("boston, ma", []string{"ma"}) {
		t.Error("'boston, ma' should match state code ma")
	}
}
