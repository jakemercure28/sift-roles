package jobfilter

import (
	"encoding/json"
	"regexp"
	"strings"
)

// This file ports the per-profile metro filtering from lib/location-filter.js
// (passesPrefs and its helpers). Picking a metro filters to jobs in that metro,
// while staying inclusive of remote and nationwide roles.

// Reason vocabulary for Evaluate: why a job failed the prefs. Callers embed
// these in archive reasons and metric labels, so keep them short and stable.
const (
	// ReasonNonUS marks a remote role that is not open to US-based candidates
	// (e.g. "Remote, United Kingdom").
	ReasonNonUS = "non_us"
	// ReasonLocation marks every other location miss: not remote under a
	// remote-only pref, an unknown location the prefs exclude, or a metro miss.
	ReasonLocation = "location"
)

// metroMatch holds the city/state matchers for one metro, parsed from metros.json.
type metroMatch struct {
	Cities []string `json:"cities"`
	States []string `json:"states"`
}

// metrosMatchConfig is metros.json decoded once for location matching (the
// city/state lists; the label is rendered separately by the dashboard).
var metrosMatchConfig = func() map[string]metroMatch {
	var m map[string]metroMatch
	_ = json.Unmarshal(metrosJSON, &m)
	return m
}()

var remoteRE = regexp.MustCompile(`(?i)\bremote\b|work from home|\bwfh\b|\banywhere\b|distributed|us[- ]only`)
var nationwideRE = regexp.MustCompile(`(?i)united states|nationwide|\busa\b`)

// usMentionRE detects an explicit US qualifier anywhere in a location string so a
// remote role tied to the US ("Remote, US", "Remote, US or Canada", "US-only")
// is kept even when a foreign country is also named. Broader than nationwideRE,
// which is only the no-city nationwide check.
var usMentionRE = regexp.MustCompile(`(?i)(^|[,\s(/|-])(u\.?s\.?a?\.?|united states(?: of america)?|stateside|conus)([,\s)/|-]|$)|nationwide`)

// nonUSCountryRE detects a non-US country or region token. Anchored on
// comma/space/paren boundaries (like stateRE) so a substring never false-matches.
// When such a token is present and no US qualifier is, an international-remote role
// ("Remote, United Kingdom") is not US-eligible and is hidden behind a US filter.
var nonUSCountryRE = regexp.MustCompile(`(?i)(^|[,\s(/|])(united kingdom|u\.?k\.?|england|scotland|wales|northern ireland|republic of ireland|ireland|germany|france|spain|portugal|italy|netherlands|belgium|switzerland|austria|sweden|norway|denmark|finland|poland|czechia|czech republic|romania|hungary|greece|bulgaria|croatia|serbia|ukraine|turkey|israel|canada|mexico|brazil|argentina|colombia|chile|india|china|japan|singapore|hong kong|taiwan|south korea|korea|australia|new zealand|philippines|indonesia|malaysia|thailand|vietnam|pakistan|nigeria|kenya|south africa|egypt|u\.?a\.?e\.?|united arab emirates|saudi arabia|qatar|emea|apac|latam|europe|european union)([,\s)/|]|$)|\bfrom eu\b|\beu[- ]only\b|\beu\b`)

// IsUSEligible reports whether a (typically remote) role is open to US-based
// candidates: it names the US, or it names no foreign country at all (a bare
// "Remote" counts as US-eligible). A foreign country with no US mention is not.
func IsUSEligible(location string) bool {
	if usMentionRE.MatchString(location) {
		return true
	}
	return !nonUSCountryRE.MatchString(location)
}

// stateREs caches a compiled `(^|[,\s])XX([,\s]|$)` matcher per state code so
// "Walthamstow, UK" does not match the MA state code via a raw substring.
var stateREs = map[string]*regexp.Regexp{}

func stateRE(st string) *regexp.Regexp {
	if re, ok := stateREs[st]; ok {
		return re
	}
	re := regexp.MustCompile(`(?i)(^|[,\s])` + regexp.QuoteMeta(st) + `([,\s]|$)`)
	stateREs[st] = re
	return re
}

// IsRemote reports whether a location/title haystack names a remote arrangement.
func IsRemote(location string) bool {
	return remoteRE.MatchString(location)
}

// looksNationwide reports a role open across the whole US: "United States" /
// "USA" / "nationwide" with no comma-separated city qualifier.
func looksNationwide(loc string) bool {
	if !nationwideRE.MatchString(loc) {
		return false
	}
	return !strings.Contains(loc, ",")
}

func matchesCity(lowerLoc string, cities []string) bool {
	for _, city := range cities {
		if strings.Contains(lowerLoc, city) {
			return true
		}
	}
	return false
}

func matchesState(lowerLoc string, states []string) bool {
	for _, st := range states {
		if stateRE(st).MatchString(lowerLoc) {
			return true
		}
	}
	return false
}

// MatchesMetro reports whether a location names a city or state of the metro.
func MatchesMetro(location, metroKey string) bool {
	metro, ok := metrosMatchConfig[metroKey]
	if !ok {
		return false
	}
	lower := strings.ToLower(location)
	if lower == "" {
		return false
	}
	return matchesCity(lower, metro.Cities) || matchesState(lower, metro.States)
}

// Passes reports whether a job's location/title clears the saved prefs.
// Inclusive by design: a chosen metro never hides remote, nationwide, or
// unknown-location roles. Mirrors passesPrefs in lib/location-filter.js.
func Passes(location, title string, prefs Prefs) bool {
	ok, _ := Evaluate(location, title, prefs)
	return ok
}

// Evaluate is Passes plus the reason a job failed, for callers (the pre-score
// prefilter) that record why a row was buried. The pass/fail decision is
// byte-for-byte the old passesPrefs logic; only the failure branches are
// annotated.
func Evaluate(location, title string, prefs Prefs) (bool, string) {
	location = strings.TrimSpace(location)
	title = strings.TrimSpace(title)

	// Multi-location postings often stash "Remote or NYC or Seattle" in the
	// title while the location field holds just one city, so scan both.
	haystack := location
	if title != "" {
		haystack = location + " | " + title
	}

	// "Remote only" means remote AND open to US-based candidates: an international
	// remote role (e.g. "Remote, United Kingdom") is not a fit for a US job search.
	if prefs.RemoteOnly {
		if !IsRemote(haystack) {
			return false, ReasonLocation
		}
		if !IsUSEligible(location) {
			return false, ReasonNonUS
		}
		return true, ""
	}

	if len(prefs.Metros) == 0 {
		return true, ""
	}
	if location == "" {
		if prefs.IncludeUnknown {
			return true, ""
		}
		return false, ReasonLocation
	}
	// A US metro filter keeps remote roles, but only US-eligible ones: "Remote, UK"
	// must not slip through just because it contains the word "remote".
	if IsRemote(haystack) && IsUSEligible(location) {
		return true, ""
	}
	if looksNationwide(location) {
		return true, ""
	}
	for _, key := range prefs.Metros {
		if MatchesMetro(haystack, key) {
			return true, ""
		}
	}
	if IsRemote(haystack) && !IsUSEligible(location) {
		return false, ReasonNonUS
	}
	return false, ReasonLocation
}
