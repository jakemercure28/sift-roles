package dashboard

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
)

// This file ports lib/location-prefs.js: the metro list and the user's saved
// location filter preferences (location.json in the data dir).

// metrosJSON is an embedded copy of config/metros.json. A test
// (locationprefs_test.go) asserts it stays identical to the repo copy.
//
//go:embed metros.json
var metrosJSON []byte

// metroKeys is the set of valid metro keys, parsed from metrosJSON once.
var metroKeys = func() map[string]struct{} {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(metrosJSON, &m)
	keys := make(map[string]struct{}, len(m))
	for k := range m {
		keys[k] = struct{}{}
	}
	return keys
}()

// MetrosRaw returns the raw metros object for API responses (matches loadMetros).
func MetrosRaw() json.RawMessage { return json.RawMessage(metrosJSON) }

// LocationPrefs mirrors the normalized prefs shape in lib/location-prefs.js.
type LocationPrefs struct {
	Metros         []string `json:"metros"`
	IncludeUnknown bool     `json:"includeUnknown"`
	RemoteOnly     bool     `json:"remoteOnly"`
}

const prefsFilename = "location.json"

// rawPrefs decodes an untrusted prefs payload. Pointers distinguish "absent"
// from an explicit false (includeUnknown defaults true, remoteOnly defaults false).
type rawPrefs struct {
	Metros         []string `json:"metros"`
	IncludeUnknown *bool    `json:"includeUnknown"`
	RemoteOnly     *bool    `json:"remoteOnly"`
}

// normalizePrefs filters metros to valid keys and applies the default rules,
// mirroring normalizePrefs. Metros is always a non-nil slice so it marshals to [].
func normalizePrefs(raw rawPrefs) LocationPrefs {
	metros := []string{}
	for _, k := range raw.Metros {
		if _, ok := metroKeys[k]; ok {
			metros = append(metros, k)
		}
	}
	includeUnknown := raw.IncludeUnknown == nil || *raw.IncludeUnknown != false
	remoteOnly := raw.RemoteOnly != nil && *raw.RemoteOnly
	return LocationPrefs{Metros: metros, IncludeUnknown: includeUnknown, RemoteOnly: remoteOnly}
}

func defaultPrefs() LocationPrefs {
	return LocationPrefs{Metros: []string{}, IncludeUnknown: true, RemoteOnly: false}
}

// loadPrefs reads dataDir/location.json, returning defaults if absent/invalid.
func loadPrefs(dataDir string) LocationPrefs {
	data, err := os.ReadFile(filepath.Join(dataDir, prefsFilename))
	if err != nil {
		return defaultPrefs()
	}
	var raw rawPrefs
	if err := json.Unmarshal(data, &raw); err != nil {
		return defaultPrefs()
	}
	return normalizePrefs(raw)
}

// savePrefs normalizes and writes prefs to dataDir/location.json (2-space indent
// + trailing newline, matching savePrefs), returning the normalized result.
func savePrefs(dataDir string, raw rawPrefs) (LocationPrefs, error) {
	prefs := normalizePrefs(raw)
	out, err := json.MarshalIndent(prefs, "", "  ")
	if err != nil {
		return prefs, err
	}
	out = append(out, '\n')
	if err := os.WriteFile(filepath.Join(dataDir, prefsFilename), out, 0o644); err != nil {
		return prefs, err
	}
	return prefs, nil
}
