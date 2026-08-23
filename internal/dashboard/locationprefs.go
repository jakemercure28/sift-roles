package dashboard

import (
	"encoding/json"

	"job-search-automation/internal/jobfilter"
)

// The metro list and saved location prefs (location.json in the data dir) live
// in internal/jobfilter; these aliases keep the dashboard-local names so the
// rest of the package reads unchanged.

// metrosJSON is the embedded copy of config/metros.json (owned by jobfilter). A
// test (locationprefs_test.go) asserts it stays identical to the repo copy.
var metrosJSON = jobfilter.MetrosJSON()

// MetrosRaw returns the raw metros object for API responses (matches loadMetros).
func MetrosRaw() json.RawMessage { return jobfilter.MetrosRaw() }

// LocationPrefs mirrors the normalized prefs shape in lib/location-prefs.js.
type LocationPrefs = jobfilter.Prefs

// rawPrefs decodes an untrusted prefs payload. Pointers distinguish "absent"
// from an explicit false (includeUnknown defaults true, remoteOnly defaults false).
type rawPrefs = jobfilter.RawPrefs

// normalizePrefs filters metros to valid keys and applies the default rules.
func normalizePrefs(raw rawPrefs) LocationPrefs { return jobfilter.Normalize(raw) }

func defaultPrefs() LocationPrefs { return jobfilter.Default() }

// loadPrefs reads dataDir/location.json, returning defaults if absent/invalid.
func loadPrefs(dataDir string) LocationPrefs { return jobfilter.Load(dataDir) }

// savePrefs normalizes and writes prefs to dataDir/location.json, returning the
// normalized result.
func savePrefs(dataDir string, raw rawPrefs) (LocationPrefs, error) {
	return jobfilter.Save(dataDir, raw)
}
