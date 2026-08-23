package discovery

// Harvest turns the global company_registry from a passive verification cache
// into an active, relevance-filtered distribution channel. Company discovery only
// ever adds a board to a tenant's scrape list when Gemini independently names it,
// so a board another tenant already verified (and that sits in the shared
// registry) stays invisible to everyone else - a merchandiser board Gemini found
// for one retail tenant never reaches the next one. Harvest closes that gap
// WITHOUT spending any LLM calls: for each verified registry board the tenant does
// not already track, it lists the board's live postings over plain HTTP and adopts
// the board only when a posting title matches one of the tenant's own search
// terms. Relevance filtering is what keeps a wider company net from ballooning the
// scoring budget, since only boards with matching open roles are ever scraped.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	// DefaultHarvestProbe bounds how many not-yet-evaluated registry boards a
	// single pass fetches, so a large registry can't stall the maintenance cron.
	DefaultHarvestProbe = 60
	// DefaultHarvestAdopt bounds how many boards one pass adds to a tenant, so a
	// broad match set trickles into the scrape (and its scoring cost) gradually.
	DefaultHarvestAdopt = 15
	harvestConcurrency  = 10
	harvestStateFile    = "harvest-state.json"
	harvestBodyLimit    = 5 << 20 // cap a board response at 5 MiB
)

// PostingsLister lists a board's open posting titles for one API platform
// (greenhouse/ashby/lever). ok reports whether the board was actually reachable
// and parseable: a false ok (transport error / non-2xx) leaves the board
// unevaluated so a later pass retries it, rather than burning it on a blip. A
// non-nil error is reserved for context cancellation and aborts the pass.
type PostingsLister func(ctx context.Context, platform, slug string) (titles []string, ok bool, err error)

// HarvestLimits bound one harvest pass. Zero values fall back to the defaults.
type HarvestLimits struct {
	MaxProbe int
	MaxAdopt int
}

// HarvestReport summarizes one harvest pass.
type HarvestReport struct {
	Probed  int
	Adopted int
	Names   []string // slugs adopted this pass, for logging
	Skipped bool
	Reason  string
}

// harvestState remembers which registry boards a tenant has already evaluated so
// repeated passes don't re-fetch the same boards every cron tick. Keyed
// "platform:slugKey".
type harvestState struct {
	Evaluated map[string]bool `json:"evaluated"`
}

// Harvest runs one registry-harvest pass for the tenant rooted at cfg.DataDir.
func Harvest(ctx context.Context, cfg Config, limits HarvestLimits) (HarvestReport, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: boardTimeout}
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "data"
	}
	if cfg.Registry == nil {
		return HarvestReport{Skipped: true, Reason: "no_registry"}, nil
	}
	if limits.MaxProbe <= 0 {
		limits.MaxProbe = DefaultHarvestProbe
	}
	if limits.MaxAdopt <= 0 {
		limits.MaxAdopt = DefaultHarvestAdopt
	}

	companyCfg, err := LoadCompanyConfig(cfg.DataDir)
	if err != nil {
		return HarvestReport{}, err
	}
	terms := normTerms(companyCfg.SearchTerms)
	if len(terms) == 0 {
		// No search terms means no relevance signal; adopting boards blind would
		// flood the tenant's scrape and scoring budget, so harvest does nothing.
		return HarvestReport{Skipped: true, Reason: "no_search_terms"}, nil
	}

	reg, err := cfg.Registry.Known(ctx)
	if err != nil {
		return HarvestReport{}, err
	}

	suggested := LoadSuggested(cfg.DataDir)
	tracked := AllSlugs(suggested)
	for key := range staticSlugSet(companyCfg) {
		tracked[key] = true
	}
	state := loadHarvestState(cfg.DataDir)

	lister := cfg.Postings
	if lister == nil {
		lister = httpPostingsLister(cfg.HTTPClient)
	}

	// Candidate registry boards: API platforms only (workday listing needs a POST
	// search, deferred), not already tracked, not already evaluated. Deterministic
	// order so adoption under MaxAdopt is stable across passes.
	type cand struct{ platform, slug string }
	var cands []cand
	collect := func(platform string, slugs []string) {
		sorted := append([]string(nil), slugs...)
		sort.Strings(sorted)
		for _, slug := range sorted {
			key := SlugKey(slug)
			if key == "" || tracked[key] || state.Evaluated[platform+":"+key] {
				continue
			}
			cands = append(cands, cand{platform, slug})
		}
	}
	collect("greenhouse", reg.Greenhouse)
	collect("ashby", reg.Ashby)
	collect("lever", reg.Lever)
	collect("smartrecruiters", reg.SmartRecruiters)
	if len(cands) > limits.MaxProbe {
		cands = cands[:limits.MaxProbe]
	}
	if len(cands) == 0 {
		return HarvestReport{Skipped: true, Reason: "nothing_new"}, nil
	}

	type outcome struct {
		matched bool
		ok      bool
	}
	results := make([]outcome, len(cands))
	if err := mapConcurrent(ctx, cands, harvestConcurrency, func(i int, c cand) error {
		titles, ok, err := lister(ctx, c.platform, c.slug)
		if err != nil {
			return err
		}
		results[i] = outcome{matched: ok && anyTitleMatches(titles, terms), ok: ok}
		return nil
	}); err != nil {
		return HarvestReport{}, err
	}

	added := 0
	var names []string
	for i, c := range cands {
		if results[i].ok {
			state.Evaluated[c.platform+":"+SlugKey(c.slug)] = true
		}
		if !results[i].matched || added >= limits.MaxAdopt {
			continue
		}
		if wasAdded, _ := addAPIBoard(&suggested, c.platform, c.slug); wasAdded {
			added++
			names = append(names, c.platform+":"+c.slug)
		}
	}

	// Save the suggested list WITHOUT touching its updatedAt: harvest is orthogonal
	// to the Gemini discovery TTL, so it must not reset (and thereby suppress) the
	// next paid discovery pass. The evaluated state is persisted regardless so
	// probed boards are not re-fetched next tick.
	if added > 0 {
		if err := SaveSuggested(cfg.DataDir, suggested); err != nil {
			return HarvestReport{}, err
		}
	}
	if err := saveHarvestState(cfg.DataDir, state); err != nil {
		cfg.Log.Warn("harvest state persist failed", "error", err)
	}
	return HarvestReport{Probed: len(cands), Adopted: added, Names: names}, nil
}

// normTerms lowercases, trims, and drops empty search terms.
func normTerms(in []string) []string {
	var out []string
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// anyTitleMatches reports whether any posting title is relevant to any term.
func anyTitleMatches(titles, terms []string) bool {
	for _, title := range titles {
		for _, term := range terms {
			if titleMatchesTerm(title, term) {
				return true
			}
		}
	}
	return false
}

var harvestNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// harvestStopTokens mirrors ats.titleStopTokens: generic seniority/role words too
// weak to carry a match on their own.
var harvestStopTokens = map[string]bool{
	"senior": true, "staff": true, "lead": true, "engineer": true, "remote": true,
}

// titleMatchesTerm mirrors ats.titleMatches (candidate=title, expected=term). It
// is copied rather than imported because ats -> db -> discovery would form an
// import cycle; keep the two in sync.
func titleMatchesTerm(title, term string) bool {
	left := strings.ToLower(title)
	right := strings.ToLower(term)
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	tokens := termTokens(term)
	if len(tokens) == 0 {
		return strings.Contains(left, right) || strings.Contains(right, left)
	}
	matched := 0
	for _, tok := range tokens {
		if strings.Contains(left, tok) {
			matched++
		}
	}
	threshold := 2
	if len(tokens) < threshold {
		threshold = len(tokens)
	}
	return matched >= threshold
}

func termTokens(term string) []string {
	lower := harvestNonAlnum.ReplaceAllString(strings.ToLower(term), " ")
	var out []string
	for _, tok := range strings.Fields(lower) {
		if len(tok) >= 3 && !harvestStopTokens[tok] {
			out = append(out, tok)
		}
	}
	return out
}

func loadHarvestState(dataDir string) harvestState {
	s := harvestState{Evaluated: map[string]bool{}}
	raw, err := os.ReadFile(filepath.Join(dataDir, harvestStateFile))
	if err != nil {
		return s
	}
	if err := json.Unmarshal(raw, &s); err != nil || s.Evaluated == nil {
		return harvestState{Evaluated: map[string]bool{}}
	}
	return s
}

func saveHarvestState(dataDir string, s harvestState) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, harvestStateFile), append(raw, '\n'), 0o644)
}

// httpPostingsLister lists a board's posting titles from the same public ATS job
// APIs discovery uses to verify boards. A non-2xx or unparseable response yields
// ok=false so the board is retried on a later pass.
func httpPostingsLister(client *http.Client) PostingsLister {
	return func(ctx context.Context, platform, slug string) ([]string, bool, error) {
		url := boardJobsURL(platform, slug)
		if url == "" {
			return nil, false, nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, false, nil
		}
		req.Header.Set("User-Agent", "Mozilla/5.0")
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			return nil, false, nil
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			io.Copy(io.Discard, resp.Body)
			return nil, false, nil
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, harvestBodyLimit))
		if err != nil {
			return nil, false, nil
		}
		titles, ok := parseBoardTitles(platform, body)
		return titles, ok, nil
	}
}

func boardJobsURL(platform, slug string) string {
	switch strings.ToLower(platform) {
	case "greenhouse":
		return "https://boards-api.greenhouse.io/v1/boards/" + slug + "/jobs"
	case "ashby":
		return "https://api.ashbyhq.com/posting-api/job-board/" + slug
	case "lever":
		return "https://api.lever.co/v0/postings/" + slug + "?mode=json"
	case "smartrecruiters":
		return "https://api.smartrecruiters.com/v1/companies/" + slug + "/postings?limit=100"
	}
	return ""
}

// parseBoardTitles extracts posting titles from a board response. ok is false when
// the body does not parse into the platform's expected shape.
func parseBoardTitles(platform string, body []byte) ([]string, bool) {
	switch strings.ToLower(platform) {
	case "greenhouse", "ashby":
		var b struct {
			Jobs []struct {
				Title string `json:"title"`
			} `json:"jobs"`
		}
		if err := json.Unmarshal(body, &b); err != nil {
			return nil, false
		}
		out := make([]string, 0, len(b.Jobs))
		for _, j := range b.Jobs {
			out = append(out, j.Title)
		}
		return out, true
	case "lever":
		var arr []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, false
		}
		out := make([]string, 0, len(arr))
		for _, j := range arr {
			out = append(out, j.Text)
		}
		return out, true
	case "smartrecruiters":
		var b struct {
			Content []struct {
				Name string `json:"name"`
			} `json:"content"`
		}
		if err := json.Unmarshal(body, &b); err != nil {
			return nil, false
		}
		out := make([]string, 0, len(b.Content))
		for _, j := range b.Content {
			out = append(out, j.Name)
		}
		return out, true
	}
	return nil, false
}
