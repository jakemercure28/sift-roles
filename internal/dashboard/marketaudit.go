package dashboard

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"job-search-automation/internal/db"
)

var marketSkillAliases = map[string][]string{
	"aws":        {"aws", "amazon web services", "ec2", "ecs", "eks", "lambda", "s3", "fargate"},
	"gcp":        {"gcp", "google cloud"},
	"azure":      {"azure"},
	"go":         {"golang"},
	"golang":     {"golang"},
	"kubernetes": {"kubernetes", "k8s"},
	"terraform":  {"terraform"},
	"ci/cd":      {"ci/cd", "cicd", "continuous integration", "continuous delivery"},
}

type marketSkillMention struct {
	Terms  []string
	Count  int
	JobIDs []string
}

func marketSkillTerms(skill string) []string {
	key := strings.ToLower(strings.TrimSpace(skill))
	aliases, ok := marketSkillAliases[key]
	if !ok {
		aliases = []string{skill}
	} else {
		aliases = append([]string{}, aliases...)
	}
	if key == "go" {
		hasGo := false
		for _, a := range aliases {
			if a == "go" {
				hasGo = true
				break
			}
		}
		if !hasGo {
			aliases = append(aliases, "go")
		}
	}
	out := []string{}
	for _, a := range aliases {
		if strings.TrimSpace(a) != "" {
			out = append(out, a)
		}
	}
	return out
}

func marketMatchesTerm(text, term string) bool {
	t := strings.ToLower(term)
	if regexp.MustCompile(`(?i)^[a-z0-9]{1,3}$`).MatchString(t) || t == "go" {
		re := regexp.MustCompile(`(?i)(^|[^a-z0-9])` + regexp.QuoteMeta(t) + `([^a-z0-9]|$)`)
		return re.MatchString(text)
	}
	return strings.Contains(strings.ToLower(text), t)
}

func countMarketSkillMentions(jobs []db.MarketSeniorityJob, skill string) marketSkillMention {
	terms := marketSkillTerms(skill)
	jobIDs := []string{}
	for _, j := range jobs {
		for _, term := range terms {
			if marketMatchesTerm(j.Description, term) {
				jobIDs = append(jobIDs, j.ID)
				break
			}
		}
	}
	return marketSkillMention{Terms: terms, Count: len(jobIDs), JobIDs: jobIDs}
}

type marketAuditSample struct {
	CurrentCount        int    `json:"currentCount"`
	AllTimeCount        int    `json:"allTimeCount"`
	CacheJobCount       *int   `json:"cacheJobCount"`
	CacheGeneratedAt    *int64 `json:"cacheGeneratedAt"`
	CacheMatchesCurrent bool   `json:"cacheMatchesCurrent"`
	LocationSource      string `json:"locationSource"`
}

type marketAuditLocation struct {
	Buckets           marketLocationBuckets             `json:"buckets"`
	Total             int                               `json:"total"`
	RemotePct         int                               `json:"remotePct"`
	TopMetros         []marketMetroCount                `json:"topMetros"`
	MetroSources      map[string][]marketLocationSource `json:"metroSources"`
	OtherLocatedCount int                               `json:"otherLocatedCount"`
	TopCities         []marketCityCount                 `json:"topCities"`
	CitySources       map[string][]marketLocationSource `json:"citySources"`
	MultiCityJobIDs   []string                          `json:"multiCityJobIds"`
	Gemini            []marketCityCount                 `json:"gemini"`
}

type marketAuditSkillSource struct {
	Skill        string   `json:"skill"`
	CachedCount  int      `json:"cachedCount"`
	CachedPct    int      `json:"cachedPct"`
	RawCount     int      `json:"rawCount"`
	TermsChecked []string `json:"termsChecked"`
	JobIDs       []string `json:"jobIds"`
	Diverges     bool     `json:"diverges"`
}

type marketAuditSkills struct {
	TopSkills    []marketSkill            `json:"topSkills"`
	SkillSources []marketAuditSkillSource `json:"skillSources"`
}

type marketAuditGap struct {
	Skill          string   `json:"skill"`
	CachedJDCount  int      `json:"cachedJdCount"`
	CachedPct      int      `json:"cachedPct"`
	RawJDMentions  int      `json:"rawJdMentions"`
	TermsChecked   []string `json:"termsChecked"`
	ResumeMentions bool     `json:"resumeMentions"`
}

type marketAuditGaps struct {
	Gaps []marketAuditGap `json:"gaps"`
}

type marketAudit struct {
	Sample   marketAuditSample   `json:"sample"`
	Location marketAuditLocation `json:"location"`
	Skills   marketAuditSkills   `json:"skills"`
	Gaps     marketAuditGaps     `json:"gaps"`
	Warnings []string            `json:"warnings"`
}

func buildMarketResearchAudit(currentJobs, jdJobs []db.MarketSeniorityJob, allTimeCount int, cache *marketCache, resumeText string, now int64) marketAudit {
	loc := computeMarketLocationBreakdown(currentJobs)
	currentCount := len(currentJobs)

	var cacheJobCount *int
	var cacheGeneratedAt *int64
	var cacheData *marketAnalysisData
	stale := false
	if cache != nil {
		cacheJobCount = cache.JobCount
		gen := cache.GeneratedAt
		cacheGeneratedAt = &gen
		cacheData = &cache.Data
		stale = now-cache.GeneratedAt > 48*3600*1000
	}

	topSkills := []marketSkill{}
	if cacheData != nil {
		topSkills = append(topSkills, cacheData.TopSkills...)
		if len(topSkills) > 20 {
			topSkills = topSkills[:20]
		}
	}
	skillSources := []marketAuditSkillSource{}
	for _, s := range topSkills {
		m := countMarketSkillMentions(jdJobs, s.Skill)
		jobIDs := m.JobIDs
		if len(jobIDs) > 50 {
			jobIDs = jobIDs[:50]
		}
		diverges := absInt(s.Count-m.Count) > max(5, jsRound(float64(s.Count)*0.25))
		skillSources = append(skillSources, marketAuditSkillSource{
			Skill: s.Skill, CachedCount: s.Count, CachedPct: s.Pct, RawCount: m.Count,
			TermsChecked: m.Terms, JobIDs: jobIDs, Diverges: diverges,
		})
	}

	resume := strings.ToLower(resumeText)
	gaps := []marketAuditGap{}
	if cacheData != nil {
		for _, g := range cacheData.GapAnalysis {
			m := countMarketSkillMentions(jdJobs, g.Skill)
			inResume := false
			for _, term := range m.Terms {
				if strings.Contains(resume, strings.ToLower(term)) {
					inResume = true
					break
				}
			}
			gaps = append(gaps, marketAuditGap{
				Skill: g.Skill, CachedJDCount: g.Count, CachedPct: g.Pct,
				RawJDMentions: m.Count, TermsChecked: m.Terms, ResumeMentions: inResume,
			})
		}
	}

	warnings := []string{}
	bucketSum := loc.Buckets.Remote + loc.Buckets.Hybrid + loc.Buckets.InPerson + loc.Buckets.NotSpecified
	if cacheJobCount != nil && *cacheJobCount != currentCount {
		warnings = append(warnings, fmt.Sprintf("Analysis cache sample (%d) differs from current market roles (%d); skills/gaps reflect the cached snapshot, location is live.", *cacheJobCount, currentCount))
	}
	if bucketSum != currentCount {
		warnings = append(warnings, fmt.Sprintf("Location buckets sum to %d but current roles = %d.", bucketSum, currentCount))
	}
	for _, c := range loc.TopCities {
		if marketIsNonCity(c.City) {
			warnings = append(warnings, fmt.Sprintf(`Top cities contains a non-city term: "%s".`, c.City))
			break
		}
	}
	if len(loc.MultiCityJobIDs) > 0 {
		warnings = append(warnings, fmt.Sprintf("%d job(s) name multiple cities and count toward each (policy: once per distinct city).", len(loc.MultiCityJobIDs)))
	}
	if stale {
		warnings = append(warnings, "Analysis cache is older than 48h; skills/gaps are from that snapshot. Location is computed live from raw rows.")
	}

	geminiCities := []marketCityCount{}
	if cacheData != nil && cacheData.LocationBreakdown != nil {
		geminiCities = cacheData.LocationBreakdown.TopCities
	}
	detSet := map[string]bool{}
	for _, c := range loc.TopCities {
		detSet[strings.ToLower(c.City)] = true
	}
	geminiOnly := []string{}
	for _, c := range geminiCities {
		if c.City != "" && !detSet[strings.ToLower(c.City)] {
			geminiOnly = append(geminiOnly, c.City)
		}
	}
	if len(geminiOnly) > 0 {
		if len(geminiOnly) > 6 {
			geminiOnly = geminiOnly[:6]
		}
		warnings = append(warnings, "Gemini listed cities not in the deterministic top set: "+strings.Join(geminiOnly, ", ")+".")
	}

	cacheMatchesCurrent := false
	if cacheJobCount != nil && *cacheJobCount == currentCount {
		cacheMatchesCurrent = true
	}
	return marketAudit{
		Sample: marketAuditSample{
			CurrentCount: currentCount, AllTimeCount: allTimeCount,
			CacheJobCount: cacheJobCount, CacheGeneratedAt: cacheGeneratedAt,
			CacheMatchesCurrent: cacheMatchesCurrent, LocationSource: "deterministic",
		},
		Location: marketAuditLocation{
			Buckets: loc.Buckets, Total: loc.Total, RemotePct: loc.RemotePct,
			TopMetros: loc.TopMetros, MetroSources: loc.MetroSources,
			OtherLocatedCount: loc.OtherLocatedCount, TopCities: loc.TopCities,
			CitySources: loc.CitySources, MultiCityJobIDs: loc.MultiCityJobIDs,
			Gemini: geminiCities,
		},
		Skills:   marketAuditSkills{TopSkills: topSkills, SkillSources: skillSources},
		Gaps:     marketAuditGaps{Gaps: gaps},
		Warnings: warnings,
	}
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func (s *Server) handleMarketResearchAudit(w http.ResponseWriter, _ *http.Request) {
	currentJobs, err := s.repo.LiveMarketSeniorityJobs()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jdJobs, err := s.repo.LiveMarketResearchJobs()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	allTimeJobs, err := s.repo.AllTimeMarketSeniorityJobs()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resumeText := ""
	if data, err := os.ReadFile(filepath.Join(s.dataDir, "resume.md")); err == nil {
		resumeText = string(data)
	}
	out, err := marshalNoHTMLEscape(buildMarketResearchAudit(currentJobs, jdJobs, len(allTimeJobs), loadMarketResearchCache(s.dataDir), resumeText, time.Now().UnixMilli()))
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}
