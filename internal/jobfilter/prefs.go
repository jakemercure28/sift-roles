// Package jobfilter owns the tenant's saved location preferences and the
// location/metro matching rules. It is a leaf package so both the dashboard
// (view-time filtering) and the scoring pipeline (pre-score prefiltering) share
// one implementation instead of drifting copies.
package jobfilter

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
)

// metrosJSON is an embedded copy of config/metros.json. A test
// (internal/dashboard/locationprefs_test.go) asserts it stays identical to the
// repo copy.
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

// MetrosJSON returns the embedded metros config bytes.
func MetrosJSON() []byte { return metrosJSON }

// MetrosRaw returns the raw metros object for API responses (matches loadMetros).
func MetrosRaw() json.RawMessage { return json.RawMessage(metrosJSON) }

// Prefs mirrors the normalized prefs shape in lib/location-prefs.js.
type Prefs struct {
	Metros         []string `json:"metros"`
	IncludeUnknown bool     `json:"includeUnknown"`
	RemoteOnly     bool     `json:"remoteOnly"`
}

const prefsFilename = "location.json"

// RawPrefs decodes an untrusted prefs payload. Pointers distinguish "absent"
// from an explicit false (includeUnknown defaults true, remoteOnly defaults false).
type RawPrefs struct {
	Metros         []string `json:"metros"`
	IncludeUnknown *bool    `json:"includeUnknown"`
	RemoteOnly     *bool    `json:"remoteOnly"`
}

// Normalize filters metros to valid keys and applies the default rules,
// mirroring normalizePrefs. Metros is always a non-nil slice so it marshals to [].
func Normalize(raw RawPrefs) Prefs {
	metros := []string{}
	for _, k := range raw.Metros {
		if _, ok := metroKeys[k]; ok {
			metros = append(metros, k)
		}
	}
	includeUnknown := raw.IncludeUnknown == nil || *raw.IncludeUnknown != false
	remoteOnly := raw.RemoteOnly != nil && *raw.RemoteOnly
	return Prefs{Metros: metros, IncludeUnknown: includeUnknown, RemoteOnly: remoteOnly}
}

// Default returns the prefs used when none are saved: no metro filter, unknown
// locations included, remote-only off. Default prefs pass every job.
func Default() Prefs {
	return Prefs{Metros: []string{}, IncludeUnknown: true, RemoteOnly: false}
}

// IsDefault reports whether prefs filter nothing, so callers can skip per-job
// evaluation entirely.
func (p Prefs) IsDefault() bool {
	return len(p.Metros) == 0 && !p.RemoteOnly
}

// Load reads dataDir/location.json, returning defaults if absent/invalid.
func Load(dataDir string) Prefs {
	data, err := os.ReadFile(filepath.Join(dataDir, prefsFilename))
	if err != nil {
		return Default()
	}
	var raw RawPrefs
	if err := json.Unmarshal(data, &raw); err != nil {
		return Default()
	}
	return Normalize(raw)
}

// Save normalizes and writes prefs to dataDir/location.json (2-space indent +
// trailing newline, matching savePrefs), returning the normalized result.
func Save(dataDir string, raw RawPrefs) (Prefs, error) {
	prefs := Normalize(raw)
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
