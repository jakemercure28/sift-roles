package dashboard

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"job-search-automation/internal/db"
)

var (
	marketRemoteRe  = regexp.MustCompile(`(?i)\bremote\b|work.from.home|\bwfh\b|\banywhere\b|\bany.?location\b`)
	marketHybridRe  = regexp.MustCompile(`(?i)\bhybrid\b`)
	marketScopeRe   = regexp.MustCompile(`(?i)\b(hybrid|on-?site|on site|in[- ]?person|in[- ]?office)\b`)
	marketEdgeRe    = regexp.MustCompile(`^[\s\-–,/|()]+|[\s\-–,/|()]+$`)
	stateTokenCache = map[string]*regexp.Regexp{}
)

var nonCityTokens = map[string]bool{
	"remote": true, "hybrid": true, "on-site": true, "onsite": true,
	"on site": true, "in-person": true, "in person": true,
	"not specified": true, "unspecified": true, "none": true, "n/a": true,
	"anywhere": true, "global": true, "worldwide": true, "various": true,
	"multiple": true, "united states": true, "usa": true, "u.s.": true,
	"u.s.a.": true, "us": true, "united states of america": true,
	"canada": true, "united kingdom": true, "uk": true, "u.k.": true,
	"ireland": true, "germany": true, "france": true, "spain": true,
	"netherlands": true, "india": true, "australia": true, "mexico": true,
	"brazil": true, "singapore": true, "japan": true, "china": true,
	"poland": true, "portugal": true, "italy": true, "sweden": true,
	"switzerland": true, "israel": true, "europe": true, "emea": true,
	"apac": true, "latam": true, "north america": true, "amer": true,
}

var cityAliases = map[string]string{
	"sf":            "San Francisco",
	"nyc":           "New York",
	"new york city": "New York",
}

type metroDef struct {
	Label  string   `json:"label"`
	Cities []string `json:"cities"`
	States []string `json:"states"`
}

var marketMetros = func() map[string]metroDef {
	var out map[string]metroDef
	_ = json.Unmarshal(metrosJSON, &out)
	return out
}()

type marketCityCount struct {
	City  string `json:"city"`
	Count int    `json:"count"`
}

type marketMetroCount struct {
	Metro string `json:"metro"`
	Count int    `json:"count"`
}

type marketLocationSource struct {
	JobID          string `json:"jobId"`
	Title          string `json:"title"`
	Company        string `json:"company"`
	RawLocation    string `json:"rawLocation"`
	NormalizedCity string `json:"normalizedCity,omitempty"`
	Metro          string `json:"metro,omitempty"`
	LocationType   string `json:"locationType"`
	Status         string `json:"status"`
	Score          *int   `json:"score"`
}

type marketLocationBuckets struct {
	Remote       int `json:"remote"`
	Hybrid       int `json:"hybrid"`
	InPerson     int `json:"in_person"`
	NotSpecified int `json:"not_specified"`
}

type marketLocationBreakdown struct {
	Buckets           marketLocationBuckets             `json:"buckets"`
	Total             int                               `json:"total"`
	RemotePct         int                               `json:"remotePct"`
	TopCities         []marketCityCount                 `json:"topCities"`
	CitySources       map[string][]marketLocationSource `json:"citySources"`
	TopMetros         []marketMetroCount                `json:"topMetros"`
	MetroSources      map[string][]marketLocationSource `json:"metroSources"`
	OtherLocatedCount int                               `json:"otherLocatedCount"`
	MultiCityJobIDs   []string                          `json:"multiCityJobIds"`
	Remote            int                               `json:"remote"`
	Hybrid            int                               `json:"hybrid"`
	InPerson          int                               `json:"in_person"`
	NotSpecified      int                               `json:"not_specified"`
	TopCitiesRender   []marketCityCount                 `json:"top_cities"`
	TopMetrosRender   []marketMetroCount                `json:"top_metros"`
}

func classifyMarketLocationType(raw string) string {
	s := strings.TrimSpace(raw)
	switch {
	case s == "":
		return "not_specified"
	case marketRemoteRe.MatchString(s):
		return "remote"
	case marketHybridRe.MatchString(s):
		return "hybrid"
	case isUsCountry(s):
		return "not_specified"
	default:
		return "in_person"
	}
}

func marketIsNonCity(token string) bool {
	return nonCityTokens[strings.ToLower(strings.TrimSpace(token))]
}

func marketCityKey(normalized string) string {
	s := strings.TrimSpace(normalized)
	if s == "" {
		return ""
	}
	s = regexp.MustCompile(`(?i)^remote\s*,\s*`).ReplaceAllString(s, "")
	if s == "" || marketIsNonCity(s) || numLocsRe.MatchString(s) {
		return ""
	}
	city := strings.TrimSpace(strings.Split(s, ",")[0])
	if city == "" || marketIsNonCity(city) {
		return ""
	}
	if alias, ok := cityAliases[strings.ToLower(city)]; ok {
		return alias
	}
	return city
}

func extractMarketCities(raw string) []string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	parts := regexp.MustCompile(`[|;/]`).Split(s, -1)
	seen := map[string]bool{}
	var out []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		scoped := marketScopeRe.ReplaceAllString(part, " ")
		scoped = marketEdgeRe.ReplaceAllString(scoped, "")
		scoped = strings.TrimSpace(scoped)
		city := marketCityKey(normalizeLocation(firstNonEmpty(scoped, part)))
		if city != "" && !seen[city] {
			seen[city] = true
			out = append(out, city)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func matchesMarketCity(lowerLoc string, cities []string) bool {
	for _, city := range cities {
		if strings.Contains(lowerLoc, city) {
			return true
		}
	}
	return false
}

func matchesMarketState(lowerLoc string, states []string) bool {
	for _, st := range states {
		if st == "" {
			continue
		}
		re := stateTokenCache[st]
		if re == nil {
			re = regexp.MustCompile(`(?i)(^|[,\s])` + regexp.QuoteMeta(st) + `([,\s]|$)`)
			stateTokenCache[st] = re
		}
		if re.MatchString(lowerLoc) {
			return true
		}
	}
	return false
}

func matchesMarketMetro(location, metroKey string) bool {
	metro, ok := marketMetros[metroKey]
	if !ok {
		return false
	}
	lower := strings.ToLower(location)
	if lower == "" {
		return false
	}
	if matchesMarketCity(lower, metro.Cities) {
		return true
	}
	return matchesMarketState(lower, metro.States)
}

func marketJobStatus(j db.MarketSeniorityJob) string {
	if j.Status != "" {
		return j.Status
	}
	if j.Stage != "" {
		return j.Stage
	}
	return "pending"
}

func computeMarketLocationBreakdown(jobs []db.MarketSeniorityJob) marketLocationBreakdown {
	buckets := marketLocationBuckets{}
	cityCounts := map[string]int{}
	citySources := map[string][]marketLocationSource{}
	metroCounts := map[string]int{}
	metroSources := map[string][]marketLocationSource{}
	multiCityIDs := []string{}
	otherLocatedCount := 0

	metroKeys := make([]string, 0, len(marketMetros))
	for k := range marketMetros {
		metroKeys = append(metroKeys, k)
	}
	sort.Strings(metroKeys)

	for _, j := range jobs {
		locType := classifyMarketLocationType(j.Location)
		switch locType {
		case "remote":
			buckets.Remote++
		case "hybrid":
			buckets.Hybrid++
		case "in_person":
			buckets.InPerson++
		default:
			buckets.NotSpecified++
		}

		cities := extractMarketCities(j.Location)
		if len(cities) > 1 {
			multiCityIDs = append(multiCityIDs, j.ID)
		}
		for _, city := range cities {
			cityCounts[city]++
			citySources[city] = append(citySources[city], marketLocationSource{
				JobID: j.ID, Title: j.Title, Company: j.Company, RawLocation: j.Location,
				NormalizedCity: city, LocationType: locType, Status: marketJobStatus(j), Score: j.Score,
			})
		}

		matched := []string{}
		for _, key := range metroKeys {
			if matchesMarketMetro(j.Location, key) {
				matched = append(matched, key)
			}
		}
		if len(matched) > 0 {
			for _, key := range matched {
				label := marketMetros[key].Label
				metroCounts[label]++
				metroSources[label] = append(metroSources[label], marketLocationSource{
					JobID: j.ID, Title: j.Title, Company: j.Company, RawLocation: j.Location,
					Metro: label, LocationType: locType, Status: marketJobStatus(j), Score: j.Score,
				})
			}
		} else if len(cities) > 0 {
			otherLocatedCount++
		}
	}

	topCities := make([]marketCityCount, 0, len(cityCounts))
	for city, count := range cityCounts {
		topCities = append(topCities, marketCityCount{City: city, Count: count})
	}
	sort.Slice(topCities, func(i, j int) bool {
		if topCities[i].Count != topCities[j].Count {
			return topCities[i].Count > topCities[j].Count
		}
		return strings.ToLower(topCities[i].City) < strings.ToLower(topCities[j].City)
	})

	topMetros := make([]marketMetroCount, 0, len(metroCounts))
	for metro, count := range metroCounts {
		topMetros = append(topMetros, marketMetroCount{Metro: metro, Count: count})
	}
	sort.Slice(topMetros, func(i, j int) bool {
		if topMetros[i].Count != topMetros[j].Count {
			return topMetros[i].Count > topMetros[j].Count
		}
		return strings.ToLower(topMetros[i].Metro) < strings.ToLower(topMetros[j].Metro)
	})

	total := len(jobs)
	remotePct := 0
	if total > 0 {
		remotePct = jsRound(float64(buckets.Remote) / float64(total) * 100)
	}
	return marketLocationBreakdown{
		Buckets: buckets, Total: total, RemotePct: remotePct,
		TopCities: topCities, CitySources: citySources, TopMetros: topMetros,
		MetroSources: metroSources, OtherLocatedCount: otherLocatedCount,
		MultiCityJobIDs: multiCityIDs,
		Remote:          buckets.Remote, Hybrid: buckets.Hybrid, InPerson: buckets.InPerson,
		NotSpecified: buckets.NotSpecified, TopCitiesRender: topCities, TopMetrosRender: topMetros,
	}
}
