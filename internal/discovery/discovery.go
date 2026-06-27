// Package discovery ports scripts/discover-companies.js. It asks Gemini for
// likely hiring companies, verifies that their public ATS boards exist, and
// appends verified boards to data/suggested-companies.json.
package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultTTLHours       = 6
	DefaultCandidateCount = 80
	MinOutputTokens       = 2000
	BootstrapThreshold    = 20
	BootstrapPasses       = 5

	boardTimeout = 10 * time.Second
	concurrency  = 15
)

var apiPlatforms = []string{"greenhouse", "ashby", "lever"}

// GeminiClient is the Gemini capability discovery needs. scorer.Client
// satisfies it via CallGemini.
type GeminiClient interface {
	CallGemini(ctx context.Context, prompt string, maxTokens int) (string, error)
}

// Registry is an optional global, cross-tenant cache of verified ATS boards. A
// verified board is a tenant-independent fact, so discovery memoizes it here:
// boards Known reports are trusted as already verified (the HTTP probe is
// skipped), and freshly probed boards are harvested via Upsert for other tenants
// to reuse. A nil Registry (self-host / SQLite single-user) leaves discovery
// behaving exactly as before: every candidate is probed and nothing is shared.
type Registry interface {
	// Known returns the boards verified recently enough to trust without
	// re-probing. The implementation applies its own freshness TTL.
	Known(ctx context.Context) (Suggested, error)
	// Upsert records freshly verified boards for cross-tenant reuse.
	Upsert(ctx context.Context, boards []VerifiedBoard) error
}

// VerifiedBoard is one board confirmed to exist, harvested into the Registry.
// For API platforms (greenhouse/ashby/lever), Platform+Slug identify it; for
// workday, Workday carries the full tenant/board coordinates.
type VerifiedBoard struct {
	Platform string
	Slug     string
	Workday  *WorkdayEntry
}

// Config controls a discovery run.
type Config struct {
	DataDir        string
	TTLHours       float64
	CandidateCount int
	Gemini         GeminiClient
	HTTPClient     *http.Client
	Registry       Registry
	Now            func() time.Time
	Log            *slog.Logger
}

// Report is the summary returned by Run.
type Report struct {
	Added          int
	TotalSuggested int
	Skipped        bool
	Reason         string
	Passes         int
	NextEligibleAt string
}

// Suggested is the persisted suggested-companies.json shape.
type Suggested struct {
	Greenhouse []string       `json:"greenhouse"`
	Ashby      []string       `json:"ashby"`
	Lever      []string       `json:"lever"`
	Workday    []WorkdayEntry `json:"workday"`
	UpdatedAt  *string        `json:"updatedAt"`
}

// WorkdayEntry identifies one Workday board.
type WorkdayEntry struct {
	Sub   string `json:"sub"`
	WD    int    `json:"wd"`
	Board string `json:"board"`
	Label string `json:"label,omitempty"`
}

// DiscoveryCandidate is one Gemini-proposed company.
type DiscoveryCandidate struct {
	Name      string `json:"name"`
	Platform  string `json:"platform"`
	Slug      string `json:"slug"`
	URL       string `json:"url"`
	Rationale string `json:"rationale"`
}

// CompanyConfig is the subset of data/companies.json needed for discovery.
type CompanyConfig struct {
	SearchTerms []string       `json:"SEARCH_TERMS"`
	Greenhouse  []string       `json:"GREENHOUSE_COMPANIES"`
	Ashby       []string       `json:"ASHBY_COMPANIES"`
	Lever       []string       `json:"LEVER_COMPANIES"`
	Workable    []string       `json:"WORKABLE_COMPANIES"`
	Rippling    []string       `json:"RIPPLING_COMPANIES"`
	Workday     []WorkdayEntry `json:"WORKDAY_COMPANIES"`
}

// HasCompanies reports whether any ATS company list is non-empty. A freshly
// onboarded tenant has search terms but empty company lists, so this is how the
// pre-scrape bootstrap decides whether discovery still needs to run for it.
func (c CompanyConfig) HasCompanies() bool {
	return len(c.Greenhouse) > 0 || len(c.Ashby) > 0 || len(c.Lever) > 0 ||
		len(c.Workable) > 0 || len(c.Rippling) > 0 || len(c.Workday) > 0
}

type cacheState struct {
	Fresh         bool
	AgeHours      *float64
	NextEligible  *time.Time
	NextEligibleS string
}

// LoadSuggested reads suggested-companies.json, defaulting missing/malformed
// files to the empty shape used by lib/suggested-companies.js.
func LoadSuggested(dataDir string) Suggested {
	raw, err := os.ReadFile(filepath.Join(dataDir, "suggested-companies.json"))
	if err != nil {
		return emptySuggested()
	}
	var s Suggested
	if err := json.Unmarshal(raw, &s); err != nil {
		return emptySuggested()
	}
	if s.Greenhouse == nil {
		s.Greenhouse = []string{}
	}
	if s.Ashby == nil {
		s.Ashby = []string{}
	}
	if s.Lever == nil {
		s.Lever = []string{}
	}
	if s.Workday == nil {
		s.Workday = []WorkdayEntry{}
	}
	return s
}

// SaveSuggested writes suggested-companies.json with the same pretty JSON +
// trailing newline contract as the Node helper.
func SaveSuggested(dataDir string, s Suggested) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(filepath.Join(dataDir, "suggested-companies.json"), raw, 0o644)
}

func emptySuggested() Suggested {
	return Suggested{
		Greenhouse: []string{},
		Ashby:      []string{},
		Lever:      []string{},
		Workday:    []WorkdayEntry{},
	}
}

// AllSlugs returns the verified board slugs counted toward discovery cache
// bootstrap state.
func AllSlugs(s Suggested) map[string]bool {
	out := map[string]bool{}
	for _, slug := range s.Greenhouse {
		if slug != "" {
			out[slug] = true
		}
	}
	for _, slug := range s.Ashby {
		if slug != "" {
			out[slug] = true
		}
	}
	for _, slug := range s.Lever {
		if slug != "" {
			out[slug] = true
		}
	}
	for _, entry := range s.Workday {
		if entry.Sub != "" {
			out[entry.Sub] = true
		}
	}
	return out
}

// WorkdayKey matches lib/suggested-companies.js: tenant plus case-sensitive
// board path.
func WorkdayKey(entry WorkdayEntry) string {
	if entry.Sub == "" || entry.Board == "" {
		return ""
	}
	return entry.Sub + "/" + entry.Board
}

// knownBoards indexes a Registry snapshot for O(1) cache lookups during a pass:
// a candidate found here is trusted as already verified, so its HTTP probe is
// skipped. The zero value is safe and never matches (nil-map reads return false),
// which is exactly the behavior when no Registry is configured.
type knownBoards struct {
	api     map[string]map[string]string // platform -> slugKey -> stored slug
	workday map[string]WorkdayEntry      // WorkdayKey -> entry
}

func buildKnown(s Suggested) knownBoards {
	k := knownBoards{api: map[string]map[string]string{}, workday: map[string]WorkdayEntry{}}
	index := func(platform string, slugs []string) {
		m := map[string]string{}
		for _, slug := range slugs {
			if slug != "" {
				m[SlugKey(slug)] = slug
			}
		}
		k.api[platform] = m
	}
	index("greenhouse", s.Greenhouse)
	index("ashby", s.Ashby)
	index("lever", s.Lever)
	for _, e := range s.Workday {
		if key := WorkdayKey(e); key != "" {
			k.workday[key] = e
		}
	}
	return k
}

// lookupAPI reports whether rawSlug matches a known API board on any platform,
// returning the stored platform+slug so the caller can adopt the canonical form.
func (k knownBoards) lookupAPI(rawSlug string) (resolvedBoard, bool) {
	key := SlugKey(rawSlug)
	for _, platform := range apiPlatforms {
		if slug, ok := k.api[platform][key]; ok {
			return resolvedBoard{Exists: true, Platform: platform, Slug: slug}, true
		}
	}
	return resolvedBoard{}, false
}

func (k knownBoards) lookupWorkday(entry WorkdayEntry) (WorkdayEntry, bool) {
	e, ok := k.workday[WorkdayKey(entry)]
	return e, ok
}

// SlugVariants mirrors slugVariants in scripts/discover-companies.js.
func SlugVariants(raw string) []string {
	base := strings.ToLower(strings.TrimSpace(raw))
	if base == "" {
		return []string{}
	}
	collapsed := regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(base, "")
	hyphenated := regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(base, "-")
	hyphenated = strings.Trim(hyphenated, "-")
	seen := map[string]bool{}
	var out []string
	for _, v := range []string{collapsed, hyphenated, base} {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// SlugKey returns the canonical collapsed slug for dedup/reject tracking.
func SlugKey(raw string) string {
	if variants := SlugVariants(raw); len(variants) > 0 {
		return variants[0]
	}
	return strings.ToLower(strings.TrimSpace(raw))
}

// MaxOutputTokens mirrors getDiscoveryMaxOutputTokens.
func MaxOutputTokens(candidateCount int) int {
	n := candidateCount * 90
	if n < MinOutputTokens {
		return MinOutputTokens
	}
	return n
}

// CacheState mirrors getDiscoveryCacheState.
func CacheState(updatedAt *string, ttl time.Duration, now time.Time) cacheState {
	if updatedAt == nil || *updatedAt == "" || ttl <= 0 {
		return cacheState{}
	}
	updated, err := time.Parse(time.RFC3339Nano, *updatedAt)
	if err != nil {
		return cacheState{}
	}
	age := now.Sub(updated)
	ageHours := float64(int((age.Hours()+0.05)*10)) / 10
	next := updated.Add(ttl)
	return cacheState{
		Fresh:         age < ttl,
		AgeHours:      &ageHours,
		NextEligible:  &next,
		NextEligibleS: jsISOString(next),
	}
}

// LoadCompanyConfig reads the active profile's data/companies.json.
func LoadCompanyConfig(dataDir string) (CompanyConfig, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, "companies.json"))
	if err != nil {
		return CompanyConfig{}, err
	}
	var cfg CompanyConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return CompanyConfig{}, err
	}
	return cfg, nil
}

var maxAgeDaysRe = regexp.MustCompile(`MAX_AGE_DAYS\s*=\s*(\d+)`)

func extractMaxAgeDays(text string) int {
	if m := maxAgeDaysRe.FindStringSubmatch(text); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n
		}
	}
	return 20
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// MigrateCompaniesFile converts a legacy CommonJS data/companies.js into
// data/companies.json (the format the worker and discovery now read). It is a
// one-time, idempotent shim: it runs only when companies.js exists and
// companies.json does not, parses the old module's arrays, writes the JSON, and
// removes the stale .js file. Returns true when a migration actually happened.
func MigrateCompaniesFile(dataDir string) (bool, error) {
	jsonPath := filepath.Join(dataDir, "companies.json")
	if _, err := os.Stat(jsonPath); err == nil {
		return false, nil
	}
	jsPath := filepath.Join(dataDir, "companies.js")
	raw, err := os.ReadFile(jsPath)
	if err != nil {
		return false, nil // nothing legacy to migrate
	}
	text := string(raw)
	workday := extractWorkdayEntries(text)
	if workday == nil {
		workday = []WorkdayEntry{}
	}
	profile := struct {
		MaxAgeDays  int            `json:"MAX_AGE_DAYS"`
		SearchTerms []string       `json:"SEARCH_TERMS"`
		Greenhouse  []string       `json:"GREENHOUSE_COMPANIES"`
		Lever       []string       `json:"LEVER_COMPANIES"`
		Workable    []string       `json:"WORKABLE_COMPANIES"`
		Ashby       []string       `json:"ASHBY_COMPANIES"`
		Workday     []WorkdayEntry `json:"WORKDAY_COMPANIES"`
		Wellfound   []string       `json:"WELLFOUND_ROLES"`
		Rippling    []string       `json:"RIPPLING_COMPANIES"`
	}{
		MaxAgeDays:  extractMaxAgeDays(text),
		SearchTerms: nonNilStrings(extractStringArray(text, "SEARCH_TERMS")),
		Greenhouse:  nonNilStrings(extractStringArray(text, "GREENHOUSE_COMPANIES")),
		Lever:       nonNilStrings(extractStringArray(text, "LEVER_COMPANIES")),
		Workable:    nonNilStrings(extractStringArray(text, "WORKABLE_COMPANIES")),
		Ashby:       nonNilStrings(extractStringArray(text, "ASHBY_COMPANIES")),
		Workday:     workday,
		Wellfound:   nonNilStrings(extractStringArray(text, "WELLFOUND_ROLES")),
		Rippling:    nonNilStrings(extractStringArray(text, "RIPPLING_COMPANIES")),
	}
	out, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(jsonPath, append(out, '\n'), 0o644); err != nil {
		return false, err
	}
	_ = os.Remove(jsPath)
	return true, nil
}

func extractStringArray(text, name string) []string {
	re := regexp.MustCompile(`(?s)\b` + regexp.QuoteMeta(name) + `\s*=\s*\[(.*?)\]`)
	match := re.FindStringSubmatch(text)
	if len(match) < 2 {
		return []string{}
	}
	itemRe := regexp.MustCompile(`'((?:\\'|[^'])*)'|"((?:\\"|[^"])*)"`)
	var out []string
	for _, m := range itemRe.FindAllStringSubmatch(match[1], -1) {
		v := m[1]
		if v == "" {
			v = m[2]
		}
		v = strings.ReplaceAll(v, `\'`, `'`)
		v = strings.ReplaceAll(v, `\"`, `"`)
		out = append(out, v)
	}
	return out
}

func extractWorkdayEntries(text string) []WorkdayEntry {
	re := regexp.MustCompile(`(?s)\bWORKDAY_COMPANIES\s*=\s*\[(.*?)\]`)
	match := re.FindStringSubmatch(text)
	if len(match) < 2 {
		return []WorkdayEntry{}
	}
	objRe := regexp.MustCompile(`(?s)\{(.*?)\}`)
	var out []WorkdayEntry
	for _, obj := range objRe.FindAllStringSubmatch(match[1], -1) {
		body := obj[1]
		entry := WorkdayEntry{
			Sub:   extractJSField(body, "sub"),
			Board: extractJSField(body, "board"),
			Label: extractJSField(body, "label"),
		}
		wd := extractJSField(body, "wd")
		if wd == "" {
			wd = extractJSField(body, "cluster")
		}
		if n, err := strconv.Atoi(wd); err == nil {
			entry.WD = n
		}
		if entry.Sub != "" && entry.WD > 0 && entry.Board != "" {
			out = append(out, entry)
		}
	}
	return out
}

func extractJSField(body, name string) string {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\s*:\s*(?:'([^']*)'|"([^"]*)"|(\d+))`)
	m := re.FindStringSubmatch(body)
	if len(m) == 0 {
		return ""
	}
	for i := 1; i < len(m); i++ {
		if m[i] != "" {
			return m[i]
		}
	}
	return ""
}

// BuildPrompt mirrors buildPrompt in scripts/discover-companies.js, using the
// resume as the primary job-seeker signal. Search terms are helpful when the
// setup wizard produced them, but they are no longer required for discovery.
func BuildPrompt(existingSlugs map[string]bool, searchTerms []string, resumeSnippet string, candidateCount int) string {
	var exclude []string
	for slug := range existingSlugs {
		exclude = append(exclude, slug)
	}
	if len(exclude) > 300 {
		exclude = exclude[:300]
	}
	resumeSection := ""
	if resumeSnippet != "" {
		resumeSection = "Job seeker resume excerpt:\n" + resumeSnippet + "\n\n"
	}
	searchSection := ""
	if len(searchTerms) > 0 {
		searchSection = "Configured search terms:\n" + strings.Join(searchTerms, ", ") + "\n\n"
	}
	platformRule := "- Only use Greenhouse, Ashby, Lever, or Workday as the platform (these have public APIs)\n" +
		"- For Greenhouse/Ashby/Lever: \"slug\" is the company's ATS board token (e.g. \"stripe\", \"cloudflare\", \"linear\")\n" +
		"- For Workday: set \"slug\" to the tenant subdomain AND set \"url\" to the full public job board URL, e.g. \"https://capitalone.wd12.myworkdayjobs.com/en-US/Capital_One\". The URL MUST contain \".wdN.myworkdayjobs.com\" and the real board path - do not invent it; omit the company if you are unsure of its exact Workday URL\n" +
		"- Include large, established employers in this field, not only startups. Major companies, institutions, hospital systems, retailers, universities, and government agencies frequently post on Workday - prefer Workday for them and provide their full Workday board URL\n"
	jsonShape := "[{\"name\":\"Company Name\",\"platform\":\"Greenhouse|Ashby|Lever|Workday\",\"slug\":\"board-slug\",\"url\":\"https://...myworkdayjobs.com/... (Workday only)\",\"rationale\":\"1 sentence\"}]\n\n"
	return "You are helping a job seeker find companies that are actively hiring.\n\n" +
		resumeSection +
		searchSection +
		"I already track these company board slugs (do NOT suggest any of them):\n" +
		strings.Join(exclude, ", ") +
		"\n\nSuggest " + strconv.Itoa(candidateCount) + " NEW companies likely to have open roles matching this job seeker. Use the resume as the primary source of industry, function, and seniority; use configured search terms only as extra hints when present." +
		"\n\nReturn strict JSON only - no markdown, no explanation:\n" +
		jsonShape +
		"Rules:\n" +
		platformRule +
		"- Focus on companies likely to have open roles matching the search terms above\n" +
		"- No consulting firms, staffing agencies, or companies that only hire through LinkedIn/Indeed\n" +
		"- Return exactly " + strconv.Itoa(candidateCount) + " candidates"
}

// Run executes company discovery once.
func Run(ctx context.Context, cfg Config) (Report, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: boardTimeout}
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "data"
	}
	if cfg.TTLHours < 0 {
		cfg.TTLHours = DefaultTTLHours
	}
	if cfg.CandidateCount <= 0 {
		cfg.CandidateCount = DefaultCandidateCount
	}

	if cfg.Gemini == nil {
		cfg.Log.Warn("GEMINI_API_KEY not set - skipping company discovery")
		return Report{Skipped: true, Reason: "gemini_not_configured"}, nil
	}

	companyCfg, err := LoadCompanyConfig(cfg.DataDir)
	if err != nil {
		return Report{}, err
	}
	suggested := LoadSuggested(cfg.DataDir)
	staticSlugs := staticSlugSet(companyCfg)
	suggestedSlugs := AllSlugs(suggested)
	isBootstrap := len(suggestedSlugs) < BootstrapThreshold
	ttl := time.Duration(cfg.TTLHours * float64(time.Hour))
	if !isBootstrap {
		state := CacheState(suggested.UpdatedAt, ttl, cfg.Now())
		if state.Fresh {
			return Report{
				Skipped:        true,
				Reason:         "cache_fresh",
				TotalSuggested: len(suggestedSlugs),
				NextEligibleAt: state.NextEligibleS,
			}, nil
		}
	}

	resumeSnippet := loadResumeSnippet(cfg.DataDir)
	// Snapshot the global registry once: boards another tenant already verified
	// are trusted here, so this run skips re-probing them.
	known := buildKnown(emptySuggested())
	if cfg.Registry != nil {
		if reg, err := cfg.Registry.Known(ctx); err != nil {
			cfg.Log.Warn("company registry load failed; verifying all candidates", "error", err)
		} else {
			known = buildKnown(reg)
		}
	}
	passes := 1
	if isBootstrap {
		passes = BootstrapPasses
	}
	totalAdded := 0
	for i := 0; i < passes; i++ {
		added, err := runDiscoveryPass(ctx, cfg, &suggested, staticSlugs, resumeSnippet, companyCfg.SearchTerms, known)
		if err != nil {
			return Report{}, err
		}
		totalAdded += added
	}

	updated := jsISOString(cfg.Now())
	suggested.UpdatedAt = &updated
	if err := SaveSuggested(cfg.DataDir, suggested); err != nil {
		return Report{}, err
	}
	state := CacheState(suggested.UpdatedAt, ttl, cfg.Now())
	return Report{
		Added:          totalAdded,
		TotalSuggested: len(AllSlugs(suggested)),
		Passes:         passes,
		NextEligibleAt: state.NextEligibleS,
	}, nil
}

func staticSlugSet(cfg CompanyConfig) map[string]bool {
	out := map[string]bool{}
	for _, slug := range cfg.Greenhouse {
		out[SlugKey(slug)] = true
	}
	for _, slug := range cfg.Ashby {
		out[SlugKey(slug)] = true
	}
	for _, slug := range cfg.Lever {
		out[SlugKey(slug)] = true
	}
	for _, entry := range cfg.Workday {
		out[SlugKey(entry.Sub)] = true
	}
	return out
}

func loadResumeSnippet(dataDir string) string {
	raw, err := os.ReadFile(filepath.Join(dataDir, "resume.md"))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 3000 {
		s = s[:3000]
	}
	return s
}

func runDiscoveryPass(ctx context.Context, cfg Config, suggested *Suggested, staticSlugs map[string]bool, resumeSnippet string, searchTerms []string, known knownBoards) (int, error) {
	suggestedSlugs := AllSlugs(*suggested)
	excluded := map[string]bool{}
	for slug := range staticSlugs {
		excluded[SlugKey(slug)] = true
	}
	for slug := range suggestedSlugs {
		excluded[SlugKey(slug)] = true
	}

	raw, err := cfg.Gemini.CallGemini(ctx, BuildPrompt(excluded, searchTerms, resumeSnippet, cfg.CandidateCount), MaxOutputTokens(cfg.CandidateCount))
	if err != nil {
		cfg.Log.Error("Gemini call failed", "error", err)
		return 0, nil
	}
	candidates := parseGeminiCandidates(raw)
	var novel []DiscoveryCandidate
	for _, c := range candidates {
		platform := strings.ToLower(strings.TrimSpace(c.Platform))
		if platform == "workday" && c.Slug == "" && c.URL != "" {
			if parsed := ParseWorkdayURL(c.URL); parsed != nil {
				c.Slug = parsed.Sub
			}
		}
		if c.Slug != "" && !excluded[SlugKey(c.Slug)] {
			c.Platform = platform
			novel = append(novel, c)
		}
	}
	if len(novel) == 0 {
		return 0, nil
	}

	type result struct {
		candidate    DiscoveryCandidate
		exists       bool
		workdayEntry *WorkdayEntry
		fromCache    bool // verified via the registry cache, not an HTTP probe
	}
	results := make([]result, len(novel))
	if err := mapConcurrent(ctx, novel, concurrency, func(i int, c DiscoveryCandidate) error {
		platform := strings.ToLower(c.Platform)
		if platform == "workday" {
			slug := strings.ToLower(c.Slug)
			c.Slug = slug
			var entry *WorkdayEntry
			if parsed := ParseWorkdayURL(c.URL); parsed != nil {
				// Trust a recently verified board from the shared registry.
				if hit, ok := known.lookupWorkday(*parsed); ok {
					results[i] = result{candidate: c, exists: true, workdayEntry: &hit, fromCache: true}
					return nil
				}
				verified, err := verifyWorkdayBoard(ctx, cfg.HTTPClient, *parsed)
				if err != nil {
					return err
				}
				entry = verified
			}
			results[i] = result{candidate: c, exists: entry != nil, workdayEntry: entry}
			return nil
		}
		if hit, ok := known.lookupAPI(c.Slug); ok {
			c.Platform = hit.Platform
			c.Slug = hit.Slug
			results[i] = result{candidate: c, exists: true, fromCache: true}
			return nil
		}
		resolved, err := resolveAPIBoard(ctx, cfg.HTTPClient, platform, c.Slug)
		if err != nil {
			return err
		}
		c.Platform = resolved.Platform
		c.Slug = resolved.Slug
		results[i] = result{candidate: c, exists: resolved.Exists}
		return nil
	}); err != nil {
		return 0, err
	}

	workdayKeys := map[string]bool{}
	for _, entry := range suggested.Workday {
		if key := WorkdayKey(entry); key != "" {
			workdayKeys[key] = true
		}
	}
	added := 0
	var harvest []VerifiedBoard
	for _, res := range results {
		if !res.exists {
			continue
		}
		c := res.candidate
		switch c.Platform {
		case "greenhouse":
			if !slices.Contains(suggested.Greenhouse, c.Slug) {
				suggested.Greenhouse = append(suggested.Greenhouse, c.Slug)
				added++
			}
		case "ashby":
			if !slices.Contains(suggested.Ashby, c.Slug) {
				suggested.Ashby = append(suggested.Ashby, c.Slug)
				added++
			}
		case "lever":
			if !slices.Contains(suggested.Lever, c.Slug) {
				suggested.Lever = append(suggested.Lever, c.Slug)
				added++
			}
		case "workday":
			if res.workdayEntry != nil {
				key := WorkdayKey(*res.workdayEntry)
				if key != "" && !workdayKeys[key] {
					suggested.Workday = append(suggested.Workday, *res.workdayEntry)
					workdayKeys[key] = true
					added++
				}
			}
		}
		// Harvest only freshly probed boards: re-recording a cache hit would
		// extend its freshness window without an actual re-verification, which
		// could keep a dead board "fresh" indefinitely.
		if res.fromCache {
			continue
		}
		if c.Platform == "workday" {
			if res.workdayEntry != nil {
				harvest = append(harvest, VerifiedBoard{Platform: "workday", Workday: res.workdayEntry})
			}
		} else {
			harvest = append(harvest, VerifiedBoard{Platform: c.Platform, Slug: c.Slug})
		}
	}
	if cfg.Registry != nil && len(harvest) > 0 {
		if err := cfg.Registry.Upsert(ctx, harvest); err != nil {
			cfg.Log.Warn("company registry upsert failed", "error", err, "count", len(harvest))
		}
	}
	return added, nil
}

func jsISOString(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func parseGeminiCandidates(raw string) []DiscoveryCandidate {
	text := strings.TrimSpace(raw)
	text = regexp.MustCompile("(?i)^```(?:json)?\\s*").ReplaceAllString(text, "")
	text = regexp.MustCompile("(?i)\\s*```\\s*$").ReplaceAllString(text, "")

	parse := func(s string) []DiscoveryCandidate {
		var direct []DiscoveryCandidate
		if err := json.Unmarshal([]byte(s), &direct); err == nil {
			return direct
		}
		var wrapped struct {
			Candidates []DiscoveryCandidate `json:"candidates"`
		}
		if err := json.Unmarshal([]byte(s), &wrapped); err == nil && wrapped.Candidates != nil {
			return wrapped.Candidates
		}
		return nil
	}
	if parsed := parse(text); parsed != nil {
		return filterCandidates(parsed)
	}
	if match := regexp.MustCompile(`(?s)\[[\s\S]*\]`).FindString(text); match != "" {
		if parsed := parse(match); parsed != nil {
			return filterCandidates(parsed)
		}
	}
	return []DiscoveryCandidate{}
}

func filterCandidates(in []DiscoveryCandidate) []DiscoveryCandidate {
	var out []DiscoveryCandidate
	for _, c := range in {
		if c.Platform != "" {
			out = append(out, c)
		}
	}
	return out
}

type resolvedBoard struct {
	Exists   bool
	Platform string
	Slug     string
}

func resolveAPIBoard(ctx context.Context, client *http.Client, guessPlatform, rawSlug string) (resolvedBoard, error) {
	guess := strings.ToLower(guessPlatform)
	var platforms []string
	seen := map[string]bool{}
	for _, p := range append([]string{guess}, apiPlatforms...) {
		if isAPIPlatform(p) && !seen[p] {
			seen[p] = true
			platforms = append(platforms, p)
		}
	}
	for _, platform := range platforms {
		for _, slug := range SlugVariants(rawSlug) {
			ok, err := boardExists(ctx, client, platform, slug)
			if err != nil {
				return resolvedBoard{}, err
			}
			if ok {
				return resolvedBoard{Exists: true, Platform: platform, Slug: slug}, nil
			}
		}
	}
	if guess == "" {
		guess = apiPlatforms[0]
	}
	return resolvedBoard{Platform: guess, Slug: SlugKey(rawSlug)}, nil
}

func isAPIPlatform(platform string) bool {
	for _, p := range apiPlatforms {
		if platform == p {
			return true
		}
	}
	return false
}

func boardExists(ctx context.Context, client *http.Client, platform, slug string) (bool, error) {
	urls := map[string]string{
		"greenhouse": "https://boards-api.greenhouse.io/v1/boards/" + slug + "/jobs",
		"ashby":      "https://api.ashbyhq.com/posting-api/job-board/" + slug,
		"lever":      "https://api.lever.co/v0/postings/" + slug + "?mode=json",
	}
	u := urls[strings.ToLower(platform)]
	if u == "" {
		return false, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, nil
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300, nil
}

// ParseWorkdayURL ports parseWorkdayUrl in scripts/discover-companies.js.
func ParseWorkdayURL(rawURL string) *WorkdayEntry {
	if rawURL == "" {
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil
	}
	hm := regexp.MustCompile(`^([^.]+)\.wd(\d+)\.myworkdayjobs\.com$`).FindStringSubmatch(strings.ToLower(u.Hostname()))
	if len(hm) == 0 {
		return nil
	}
	wd, err := strconv.Atoi(hm[2])
	if err != nil {
		return nil
	}
	segs := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	board := ""
	for i, seg := range segs {
		if seg == "cxs" && i+2 < len(segs) {
			board = segs[i+2]
			break
		}
	}
	if board == "" {
		localeRe := regexp.MustCompile(`^[a-z]{2}-[A-Z]{2}$`)
		for _, seg := range segs {
			if !localeRe.MatchString(seg) {
				board = seg
				break
			}
		}
	}
	if board == "" {
		return nil
	}
	return &WorkdayEntry{Sub: hm[1], WD: wd, Board: board}
}

func verifyWorkdayBoard(ctx context.Context, client *http.Client, entry WorkdayEntry) (*WorkdayEntry, error) {
	if entry.Sub == "" || entry.WD <= 0 || entry.Board == "" {
		return nil, nil
	}
	u := fmt.Sprintf("https://%s.wd%d.myworkdayjobs.com/wday/cxs/%s/%s/jobs", entry.Sub, entry.WD, entry.Sub, entry.Board)
	body := bytes.NewBufferString(`{"limit":1,"offset":0,"searchText":"","appliedFacets":{}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, nil
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	var parsed struct {
		Total *int `json:"total"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.Total == nil {
		return nil, nil
	}
	return &entry, nil
}

func mapConcurrent[T any](ctx context.Context, items []T, limit int, fn func(int, T) error) error {
	if limit <= 0 {
		limit = 1
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	next := 0
	var mu sync.Mutex
	worker := func() {
		defer wg.Done()
		for {
			if ctx.Err() != nil {
				return
			}
			mu.Lock()
			if next >= len(items) {
				mu.Unlock()
				return
			}
			i := next
			next++
			item := items[i]
			mu.Unlock()
			if err := fn(i, item); err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
		}
	}
	n := limit
	if len(items) < n {
		n = len(items)
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go worker()
	}
	wg.Wait()
	select {
	case err := <-errCh:
		return err
	default:
		return ctx.Err()
	}
}
