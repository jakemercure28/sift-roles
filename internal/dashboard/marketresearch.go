package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"job-search-automation/internal/db"

	"golang.org/x/sync/errgroup"
)

type marketSkill struct {
	Skill string `json:"skill"`
	Count int    `json:"count"`
	Pct   int    `json:"pct"`
}

type marketGap struct {
	Skill string `json:"skill"`
	Count int    `json:"count"`
	Pct   int    `json:"pct"`
}

type marketStrength struct {
	Skill string `json:"skill"`
	Count int    `json:"count"`
}

type marketSignal struct {
	Term     string `json:"term"`
	JobCount int    `json:"job_count"`
	Note     string `json:"note"`
}

type marketStrategy struct {
	PoleALabel string   `json:"pole_a_label"`
	PoleBLabel string   `json:"pole_b_label"`
	PoleAPct   *float64 `json:"pole_a_pct"`
	PoleBPct   *float64 `json:"pole_b_pct"`
	IDPPct     *float64 `json:"idp_pct"`
	OpsPct     *float64 `json:"ops_pct"`
	Lean       string   `json:"lean"`
}

type marketAnalysisData struct {
	Summary           string                   `json:"summary"`
	SampleSize        int                      `json:"sample_size"`
	TopSkills         []marketSkill            `json:"top_skills"`
	GapAnalysis       []marketGap              `json:"gap_analysis"`
	ResumeStrengths   []marketStrength         `json:"resume_strengths"`
	StrategyScore     *marketStrategy          `json:"strategy_score"`
	EmergingHighScore []marketSignal           `json:"emerging_high_score"`
	LocationBreakdown *marketGeminiLocationSet `json:"location_breakdown"`
}

type marketCache struct {
	GeneratedAt   int64              `json:"generatedAt"`
	LastAttemptAt int64              `json:"lastAttemptAt,omitempty"`
	JobCount      *int               `json:"jobCount"`
	Data          marketAnalysisData `json:"data"`
}

// MarketResearchRefresh reports the outcome of a market-research cache refresh.
type MarketResearchRefresh struct {
	JobCount  int
	Skipped   bool
	Reason    string
	CachePath string
}

type marketGeminiLocationSet struct {
	TopCities []marketCityCount `json:"top_cities"`
}

type marketResearchDataSet struct {
	JobCount int
	JDCount  int
	Jobs     []db.MarketSeniorityJob
}

type marketResearchPageData struct {
	Cache         *marketCache
	JobCount      int
	AllJobs       []db.MarketSeniorityJob
	ApplicantYoe  *int
	AppliedCount  int
	Current       marketResearchDataSet
	AllTime       marketResearchDataSet
	AnalysisError string
}

func loadMarketResearchCache(dataDir string) *marketCache {
	data, err := os.ReadFile(filepath.Join(dataDir, "market-research-cache.json"))
	if err != nil {
		return nil
	}
	var cache marketCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil
	}
	return &cache
}

const marketResearchCacheTTL = 23 * time.Hour

func (s *Server) loadMarketResearchPageData(analysisError string) (marketResearchPageData, error) {
	var (
		currentJobs    []db.MarketSeniorityJob
		allTimeJobs    []db.MarketSeniorityJob
		liveCount      int
		allTimeJDCount int
		appliedCount   int
	)
	var g errgroup.Group
	g.Go(func() error {
		var err error
		currentJobs, err = s.repo.LiveMarketSeniorityJobs()
		return err
	})
	g.Go(func() error {
		var err error
		allTimeJobs, err = s.repo.AllTimeMarketSeniorityJobs()
		return err
	})
	g.Go(func() error {
		var err error
		liveCount, err = s.repo.CountLiveMarketResearchJobs()
		return err
	})
	g.Go(func() error {
		var err error
		allTimeJDCount, err = s.repo.CountAllTimeMarketResearchJobs()
		return err
	})
	g.Go(func() error {
		var err error
		appliedCount, err = s.repo.CountAllTimeAppliedJobs()
		return err
	})
	if err := g.Wait(); err != nil {
		return marketResearchPageData{}, err
	}
	return marketResearchPageData{
		Cache:        loadMarketResearchCache(s.dataDir),
		JobCount:     liveCount,
		AllJobs:      currentJobs,
		ApplicantYoe: computeApplicantYoe(s.dataDir, time.Now()),
		AppliedCount: appliedCount,
		Current: marketResearchDataSet{
			JobCount: liveCount,
			JDCount:  liveCount,
			Jobs:     currentJobs,
		},
		AllTime: marketResearchDataSet{
			JobCount: len(allTimeJobs),
			JDCount:  allTimeJDCount,
			Jobs:     allTimeJobs,
		},
		AnalysisError: analysisError,
	}, nil
}

// renderMarketResearchBody returns the Market Research page body, served from an
// in-memory cache keyed by a cheap jobs-table signature. A request carrying an
// analysisError is always rendered fresh and never cached, since the banner is
// request-specific.
func (s *Server) renderMarketResearchBody(analysisError string) (string, error) {
	html, _, err := s.renderMarketResearchBodyStatus(analysisError)
	return html, err
}

func (s *Server) renderMarketResearchBodyStatus(analysisError string) (string, string, error) {
	if analysisError != "" {
		data, err := s.loadMarketResearchPageData(analysisError)
		if err != nil {
			return "", "bypass", err
		}
		return renderMarketResearch(data), "bypass", nil
	}

	count, maxUpdated, err := s.repo.MarketResearchSignature()
	if err != nil {
		return "", "error", err
	}
	sig := s.repo.UserID() + "|" + strconv.Itoa(count) + "|" + maxUpdated
	key := s.repo.UserID() + "|market-research"
	build := func() (string, error) {
		data, err := s.loadMarketResearchPageData("")
		if err != nil {
			return "", err
		}
		return renderMarketResearch(data), nil
	}
	if s.market == nil {
		html, err := build()
		return html, "disabled", err
	}
	html, hit, err := s.market.get(key, sig, build)
	return html, cacheLabel(hit), err
}

func buildMarketResearchPrompt(jobs []db.MarketSeniorityJob, resume string) string {
	blocks := make([]string, 0, len(jobs))
	for i, j := range jobs {
		score := "null"
		if j.Score != nil {
			score = strconv.Itoa(*j.Score)
		}
		location := j.Location
		if location == "" {
			location = "not specified"
		}
		desc := j.Description
		if len(desc) > 600 {
			desc = desc[:600]
		}
		blocks = append(blocks, "[JD "+strconv.Itoa(i+1)+"] "+j.Company+" — "+j.Title+" (score:"+score+", location:"+location+")\n"+desc)
	}
	jdBlock := strings.Join(blocks, "\n\n---\n\n")
	return `You are a job market analyst. Analyze these ` + strconv.Itoa(len(jobs)) + ` job descriptions for the candidate's target role and industry (infer the field from the candidate's resume and the job descriptions, do not assume software or DevOps) and compare them against the candidate's resume.

CANDIDATE RESUME:
` + resume + `

JOB DESCRIPTIONS (each prefixed with score 1-10, where 10 = best fit):
` + jdBlock + `

LANGUAGE RULES (apply to every text field: summary, lean_note, and all note fields):
- Write plain, grounded, evidence-based English. Cite real counts or percentages from the JDs ("28 of 312 JDs mention X", "X appears in 23%").
- Do NOT use marketing or thought-leadership phrasing. Banned words/phrases: agentic (as a mindset/adjective), AI-native, orchestration at scale, high-value roles, key methodology, heavily shifting, market is heavily tilting, innovation, leverage, strategic, unlock, next-generation, emerging signals, thought leadership, paradigm, cutting-edge, transform.
- Technical terms that literally appear in the job descriptions (Kubernetes, Terraform, agentic systems if a JD says it, GPU, etc.) are allowed. The ban is on hype framing, not on real terminology.
- State what the data shows, then what it implies for this candidate. No filler.

Return ONLY a valid JSON object (no markdown, no explanation, no code fences). Schema:
{
  "summary": "2-3 plain sentences: what these JDs most ask for, and where this candidate already fits vs needs proof. Cite counts/percentages. Follow the LANGUAGE RULES.",
  "top_skills": [{"skill": "string", "count": number, "pct": number}],
  "gap_analysis": [{"skill": "string", "count": number, "pct": number, "note": "brief explanation"}],
  "resume_strengths": [{"skill": "string", "count": number}],
  "trending": ["string"],
  "location_breakdown": {"remote": number, "hybrid": number, "in_person": number, "not_specified": number, "top_cities": [{"city": "string", "count": number}]},
  "sample_size": ` + strconv.Itoa(len(jobs)) + `,
  "strategy_score": {
    "axis_question": "string",
    "pole_a_label": "string",
    "pole_a_sub": "string",
    "pole_b_label": "string",
    "pole_b_sub": "string",
    "pole_a_pct": number,
    "pole_b_pct": number,
    "lean_direction": "a | b | balanced",
    "lean_note": "string"
  },
  "emerging_high_score": [
    {
      "term": "string",
      "job_count": number,
      "note": "string"
    }
  ]
}

Rules:
- top_skills: top 20 skills/technologies by frequency across all JDs, sorted by count desc. count = number of JDs mentioning it, pct = percentage of total JDs.
  IMPORTANT: Track "ECS/Fargate" as its own explicit skill. Count a JD toward "ECS/Fargate" only if it specifically mentions ECS, Fargate, ECS Fargate, or Amazon ECS. Generic AWS mentions (Lambda, S3, IAM, etc.) without ECS/Fargate do NOT count. A JD mentioning Fargate counts for BOTH "AWS" and "ECS/Fargate".
- gap_analysis: skills appearing in >= 15% of JDs that are NOT present or underrepresented on the resume. Max 10 items. Sorted by count desc.
- resume_strengths: skills from the resume that appear in >= 20% of JDs. Max 10 items. Sorted by count desc.
- trending: 5-8 emerging/newer terms or concepts appearing in JDs that signal where the market is heading in 2026. These should be things like new frameworks, methodologies, or terminology not yet mainstream.
- location_breakdown: categorize each JD's location field. "remote" = fully remote (includes "Remote", "Work from Home", "Anywhere"). "hybrid" = mix of remote and office days mentioned. "in_person" = on-site only, no remote option. "not_specified" = location field is blank, null, or ambiguous. top_cities: list the top 10 most common specific cities/metros mentioned across all JDs, each with a count of how many JDs mention that city/metro.
- strategy_score: Identify the TWO dominant, opposing role archetypes that these JDs split between for THIS candidate's field/industry (inferred from the resume and JDs). Choose poles that genuinely fit the roles being analyzed, do not assume software or DevOps. For each JD, decide which pole it leans toward. pole_a_label and pole_b_label = short archetype names (2-4 words each). pole_a_sub and pole_b_sub = a few representative focuses or skills for each pole. pole_a_pct = % of JDs leaning toward pole A, pole_b_pct = % leaning toward pole B. These should sum to ~100. axis_question = a one-sentence "Are JDs asking for {pole A} or {pole B}?" style framing. lean_direction = "a" if pole_a_pct > 55, "b" if pole_b_pct > 55, else "balanced". lean_note = 1 plain sentence on what this split means for which roles to target (follow the LANGUAGE RULES; no hype).
- emerging_high_score: Look specifically at JDs with score >= 9. Find terms, concepts, or technologies that appear in those high-score JDs but are rare or absent in lower-scored JDs. These are signals of what employers most value in 2026. Up to 8 terms, sorted by job_count desc. term = the keyword/concept, job_count = how many score-9+ JDs contain it, note = 1 plain sentence stating how common it is in 9+ roles vs lower-scored ones, so why it is likely a differentiator (follow the LANGUAGE RULES; no hype).
- All counts and pcts must be real numbers based on actual analysis of the JDs provided.`
}

var codeFenceJSONRe = regexp.MustCompile(`(?is)^` + "```" + `(?:json)?\s*|\s*` + "```" + `$`)

func parseMarketGeminiJSON(text string) (marketAnalysisData, error) {
	cleaned := strings.TrimSpace(text)
	cleaned = strings.TrimSpace(codeFenceJSONRe.ReplaceAllString(cleaned, ""))
	var data marketAnalysisData
	if err := json.Unmarshal([]byte(cleaned), &data); err == nil {
		return data, nil
	}
	start := strings.Index(cleaned, "{")
	end := strings.LastIndex(cleaned, "}")
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(cleaned[start:end+1]), &data); err == nil {
			return data, nil
		}
	}
	return marketAnalysisData{}, errors.New("invalid Gemini JSON")
}

func marshalIndentNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return out, nil
}

func (s *Server) handleMarketResearchRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/?filter=market-research", http.StatusFound)
}

func (s *Server) handleMarketResearch(w http.ResponseWriter, r *http.Request) {
	if _, err := s.RefreshMarketResearch(r.Context(), true); err != nil {
		msg := "Analysis could not run: " + err.Error()
		if regexp.MustCompile(`(?i)exceeded your current quota|RESOURCE_EXHAUSTED|quota`).MatchString(err.Error()) {
			msg = "Gemini free-tier daily limit reached (500/day). Try again tomorrow."
		}
		s.redirectMarketResearchError(w, r, msg)
		return
	}
	http.Redirect(w, r, "/?filter=market-research", http.StatusFound)
}

// RefreshMarketResearch ports scripts/run-market-research.js for the Go
// maintenance loop. It refreshes market-research-cache.json unless the cache is
// still fresh for the same JD sample size; force bypasses that TTL for the
// dashboard's manual rerun button.
func (s *Server) RefreshMarketResearch(ctx context.Context, force bool) (MarketResearchRefresh, error) {
	cachePath := filepath.Join(s.dataDir, "market-research-cache.json")

	// Gate on the cheap cohort COUNT before reading any descriptions. The cache's
	// validity is keyed on the live-JD count, so a fresh cache or a recent-failure
	// backoff can be decided without LiveMarketResearchJobs, which pulls every live
	// job's (truncated) description over the remote pooler. Only a genuine
	// regenerate below pays for that read. CountLiveMarketResearchJobs scans the
	// exact same cohort as LiveMarketResearchJobs, so the count it returns equals
	// the row count the read would have produced.
	count, err := s.repo.CountLiveMarketResearchJobs()
	if err != nil {
		return MarketResearchRefresh{}, err
	}
	if count == 0 {
		return MarketResearchRefresh{
			JobCount:  0,
			Skipped:   true,
			Reason:    "no_jobs",
			CachePath: cachePath,
		}, nil
	}
	if !force {
		if cache := loadMarketResearchCache(s.dataDir); cache != nil {
			// Fresh successful cache: same job count and generated within the TTL.
			if cache.GeneratedAt > 0 && cache.JobCount != nil && *cache.JobCount == count {
				if age := time.Since(time.UnixMilli(cache.GeneratedAt)); age >= 0 && age < marketResearchCacheTTL {
					return MarketResearchRefresh{
						JobCount:  count,
						Skipped:   true,
						Reason:    "cache_fresh",
						CachePath: cachePath,
					}, nil
				}
			}
			// Recent failed attempt: back off for the same TTL so a bad Gemini
			// response (e.g. invalid JSON) doesn't burn quota on every maintenance run.
			if cache.LastAttemptAt > 0 && cache.GeneratedAt == 0 {
				if age := time.Since(time.UnixMilli(cache.LastAttemptAt)); age >= 0 && age < marketResearchCacheTTL {
					return MarketResearchRefresh{
						JobCount:  count,
						Skipped:   true,
						Reason:    "recent_failure_backoff",
						CachePath: cachePath,
					}, nil
				}
			}
		}
	}

	jobs, err := s.repo.LiveMarketResearchJobs()
	if err != nil {
		return MarketResearchRefresh{}, err
	}
	if len(jobs) == 0 {
		// The cohort emptied between the count and this read; treat as no_jobs.
		return MarketResearchRefresh{
			JobCount:  0,
			Skipped:   true,
			Reason:    "no_jobs",
			CachePath: cachePath,
		}, nil
	}
	resume := ""
	if data, err := os.ReadFile(filepath.Join(s.dataDir, "resume.md")); err == nil {
		resume = string(data)
	}
	sc := s.scorer()
	if sc == nil {
		return MarketResearchRefresh{}, errors.New("Gemini scorer is not configured")
	}
	raw, err := sc.Ask(ctx, buildMarketResearchPrompt(jobs, resume), 5000)
	if err != nil {
		if !force {
			s.writeMarketResearchFailedAttempt(cachePath)
		}
		return MarketResearchRefresh{}, err
	}
	data, err := parseMarketGeminiJSON(raw)
	if err != nil {
		if !force {
			s.writeMarketResearchFailedAttempt(cachePath)
		}
		return MarketResearchRefresh{}, err
	}
	jobCount := len(jobs)
	cache := marketCache{GeneratedAt: time.Now().UnixMilli(), JobCount: &jobCount, Data: data}
	out, err := marshalIndentNoHTMLEscape(cache)
	if err != nil {
		return MarketResearchRefresh{}, err
	}
	if err := os.WriteFile(cachePath, out, 0o644); err != nil {
		return MarketResearchRefresh{}, err
	}
	return MarketResearchRefresh{JobCount: jobCount, CachePath: cachePath}, nil
}

// writeMarketResearchFailedAttempt records a sentinel cache entry so the TTL
// check in RefreshMarketResearch suppresses retries after a failed Gemini call.
// It never overwrites a file that already has a successful GeneratedAt stamp.
func (s *Server) writeMarketResearchFailedAttempt(cachePath string) {
	if existing := loadMarketResearchCache(s.dataDir); existing != nil && existing.GeneratedAt > 0 {
		return
	}
	sentinel := marketCache{LastAttemptAt: time.Now().UnixMilli()}
	if out, err := marshalIndentNoHTMLEscape(sentinel); err == nil {
		_ = os.WriteFile(cachePath, out, 0o644)
	}
}

func (s *Server) redirectMarketResearchError(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/?filter=market-research&analysisError="+url.QueryEscape(msg), http.StatusFound)
}
