package ats

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// GeminiClient is satisfied by internal/scorer.Client. It is kept as a small
// interface so resolver tests can inject a deterministic fake.
type GeminiClient interface {
	CallGemini(ctx context.Context, prompt string, maxTokens int) (string, error)
}

// Resolution is the output of resolving an alternate/aggregator job into a
// primary ATS posting, ported from the `resolution()` object in lib/ats-resolver.js.
type Resolution struct {
	Status     string
	Platform   string
	URL        string
	Job        *CanonicalJob
	Confidence float64
	Evidence   map[string]any
}

func resolution(status string, fields Resolution) *Resolution {
	if fields.Evidence == nil {
		fields.Evidence = map[string]any{}
	}
	fields.Status = status
	return &fields
}

// ResolveOptions controls network and Gemini dependencies.
type ResolveOptions struct {
	Fetch  Fetcher
	Gemini GeminiClient
}

func (o ResolveOptions) fetcher() Fetcher {
	if o.Fetch != nil {
		return o.Fetch
	}
	return httpFetcher(nil)
}

// resolvePrimaryURL fetches and verifies a direct primary ATS URL.
func resolvePrimaryURL(ctx context.Context, fetch Fetcher, rawURL string, job Job) (*Resolution, error) {
	primary := primaryFromURL(rawURL)
	if primary == nil {
		return nil, nil
	}
	canonicalJob, err := fetchPrimaryJob(ctx, fetch, primary.platform, rawURL, job)
	if err != nil {
		return nil, err
	}
	if canonicalJob == nil || canonicalJob.ID == "" {
		return nil, nil
	}
	return resolution("primary", Resolution{
		Platform:   primary.displayPlatform,
		URL:        rawURL,
		Job:        canonicalJob,
		Confidence: 0.95,
		Evidence: map[string]any{
			"method":    "direct-url",
			"sourceUrl": rawURL,
		},
	}), nil
}

// flexibleString decodes JSON strings, numbers, and booleans into a string,
// matching JavaScript's loose object handling in candidateUrl().
type flexibleString string

func (s *flexibleString) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		*s = ""
		return nil
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		*s = flexibleString(str)
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*s = flexibleString(fmt.Sprint(value))
	return nil
}

func (s flexibleString) String() string { return string(s) }

// GeminiCandidate is one candidate object returned by the model. The alternate
// field names mirror candidateUrl()/verifyGeminiCandidate() in the JS resolver.
type GeminiCandidate struct {
	Platform    flexibleString `json:"platform"`
	ATS         flexibleString `json:"ats"`
	URL         flexibleString `json:"url"`
	JobURL      flexibleString `json:"jobUrl"`
	PostingURL  flexibleString `json:"postingUrl"`
	Slug        flexibleString `json:"slug"`
	CompanySlug flexibleString `json:"companySlug"`
	Board       flexibleString `json:"board"`
	BoardToken  flexibleString `json:"boardToken"`
	JobID       flexibleString `json:"jobId"`
	ID          flexibleString `json:"id"`
	JobIDSnake  flexibleString `json:"job_id"`
	Company     flexibleString `json:"company"`
	Title       flexibleString `json:"title"`
	Rationale   flexibleString `json:"rationale"`
}

func (c GeminiCandidate) platformValue() string {
	return firstNonEmpty(c.Platform.String(), c.ATS.String())
}

func (c GeminiCandidate) urlValue() string {
	return firstNonEmpty(c.URL.String(), c.JobURL.String(), c.PostingURL.String())
}

func (c GeminiCandidate) slugValue() string {
	return firstNonEmpty(c.Slug.String(), c.CompanySlug.String(), c.Board.String(), c.BoardToken.String())
}

func (c GeminiCandidate) jobIDValue() string {
	return firstNonEmpty(c.JobID.String(), c.ID.String(), c.JobIDSnake.String())
}

func (c GeminiCandidate) evidence() map[string]any {
	return map[string]any{
		"platform":  c.platformValue(),
		"slug":      c.slugValue(),
		"url":       c.urlValue(),
		"company":   c.Company.String(),
		"title":     c.Title.String(),
		"rationale": c.Rationale.String(),
	}
}

// parseGeminiJSONArray extracts the model's candidate array from strict JSON,
// {"candidates":[...]}, fenced JSON, or text containing a JSON array.
func parseGeminiJSONArray(raw string) []GeminiCandidate {
	text := strings.TrimSpace(raw)
	text = regexp.MustCompile("(?i)^```(?:json)?\\s*").ReplaceAllString(text, "")
	text = regexp.MustCompile("(?i)\\s*```\\s*$").ReplaceAllString(text, "")

	parse := func(s string) []GeminiCandidate {
		var direct []GeminiCandidate
		if err := json.Unmarshal([]byte(s), &direct); err == nil {
			return direct
		}
		var wrapped struct {
			Candidates []GeminiCandidate `json:"candidates"`
		}
		if err := json.Unmarshal([]byte(s), &wrapped); err == nil && wrapped.Candidates != nil {
			return wrapped.Candidates
		}
		return nil
	}

	if parsed := parse(text); parsed != nil {
		return parsed
	}
	if match := regexp.MustCompile(`(?s)\[[\s\S]*\]`).FindString(text); match != "" {
		if parsed := parse(match); parsed != nil {
			return parsed
		}
	}
	return []GeminiCandidate{}
}

// candidateURL builds a canonical URL from Gemini fields when one is not
// supplied directly.
func candidateURL(candidate GeminiCandidate) string {
	platform := normalizePlatform(candidate.platformValue())
	if url := candidate.urlValue(); url != "" {
		return url
	}
	slug := candidate.slugValue()
	jobID := candidate.jobIDValue()
	if slug == "" || jobID == "" {
		return ""
	}
	switch platform {
	case "greenhouse":
		return fmt.Sprintf("https://job-boards.greenhouse.io/%s/jobs/%s", slug, jobID)
	case "lever":
		return fmt.Sprintf("https://jobs.lever.co/%s/%s", slug, jobID)
	case "ashby":
		return fmt.Sprintf("https://jobs.ashbyhq.com/%s/%s", slug, jobID)
	}
	return ""
}

func verifyGeminiCandidate(ctx context.Context, fetch Fetcher, candidate GeminiCandidate, job Job) (*CanonicalJob, error) {
	platform := normalizePlatform(candidate.platformValue())
	if !isPrimaryPlatform(platform) {
		return nil, nil
	}
	if url := candidateURL(candidate); url != "" {
		canonical, err := fetchPrimaryJob(ctx, fetch, platform, url, job)
		if err != nil {
			return nil, err
		}
		if canonical != nil && canonical.ID != "" && titleMatches(canonical.Title, job.Title) {
			return canonical, nil
		}
	}

	slug := candidate.slugValue()
	if slug == "" {
		return nil, nil
	}
	switch platform {
	case "greenhouse":
		return searchGreenhouseBoard(ctx, fetch, job, slug)
	case "ashby":
		return searchAshbyBoard(ctx, fetch, job, slug)
	case "lever":
		return searchLeverBoard(ctx, fetch, job, slug)
	}
	return nil, nil
}

// buildGeminiPrompt asks Gemini for possible primary ATS candidates. The text is
// ported verbatim from buildGeminiPrompt in lib/ats-resolver.js.
func buildGeminiPrompt(job Job) string {
	return fmt.Sprintf(`Find possible canonical ATS postings for this job. Return strict JSON only, no markdown.

Expected JSON shape:
[{"platform":"Greenhouse|Ashby|Lever|Workday","slug":"company-board-slug if known","url":"posting URL if known","company":"company name","title":"job title","rationale":"short reason"}]

Rules:
- Include at most 5 candidates.
- Only use Greenhouse, Ashby, Lever, or Workday.
- Do not claim certainty. These will be verified by API before use.
- Prefer exact title/company matches.

Job:
Platform: %s
Company: %s
Title: %s
URL: %s
Location: %s`, job.Platform, job.Company, job.Title, job.URL, job.Location)
}

func proposeGeminiCandidates(ctx context.Context, gemini GeminiClient, job Job) []GeminiCandidate {
	if gemini == nil {
		return []GeminiCandidate{}
	}
	raw, err := gemini.CallGemini(ctx, buildGeminiPrompt(job), 1200)
	if err != nil {
		return []GeminiCandidate{}
	}
	candidates := parseGeminiJSONArray(raw)
	if len(candidates) > 5 {
		candidates = candidates[:5]
	}
	return candidates
}

func resolveViaGemini(ctx context.Context, fetch Fetcher, gemini GeminiClient, job Job) (*Resolution, error) {
	candidates := proposeGeminiCandidates(ctx, gemini, job)
	for _, candidate := range candidates {
		canonical, err := verifyGeminiCandidate(ctx, fetch, candidate, job)
		if err != nil {
			return nil, err
		}
		if canonical == nil || canonical.ID == "" {
			continue
		}
		return resolution("primary", Resolution{
			Platform:   canonical.Platform,
			URL:        canonical.URL,
			Job:        canonical,
			Confidence: 0.78,
			Evidence: map[string]any{
				"method":    "gemini-candidate-api-verified",
				"candidate": candidate.evidence(),
			},
		}), nil
	}
	if len(candidates) > 0 {
		return resolution("unresolved", Resolution{
			Evidence: map[string]any{
				"reason":         "gemini-candidates-unverified",
				"candidateCount": len(candidates),
			},
		}), nil
	}
	return nil, nil
}

// ResolveAlternateJob resolves an alternate ATS/aggregator job into a canonical
// primary posting. It ports resolveAlternateJob from lib/ats-resolver.js.
func ResolveAlternateJob(ctx context.Context, job Job, options ResolveOptions) (*Resolution, error) {
	fetch := options.fetcher()
	if job.URL == "" {
		return resolution("unresolved", Resolution{
			Evidence: map[string]any{"reason": "missing-url"},
		}), nil
	}

	if isPrimaryPlatform(job.Platform) {
		direct, err := resolvePrimaryURL(ctx, fetch, job.URL, job)
		if err != nil || direct != nil {
			return direct, err
		}
	}

	direct, err := resolvePrimaryURL(ctx, fetch, job.URL, job)
	if err != nil || direct != nil {
		return direct, err
	}

	page, err := fetchText(ctx, fetch, job.URL, "resolve-page/"+firstNonEmpty(job.ID, job.URL))
	if err != nil {
		return nil, err
	}
	var candidateURLs []string
	if page != nil {
		candidateURLs = extractCandidateURLs(page.Text, page.FinalURL)
	}

	var unsupported []string
	unsupportedSeen := map[string]bool{}
	addUnsupported := func(name string) {
		if name != "" && !unsupportedSeen[name] {
			unsupportedSeen[name] = true
			unsupported = append(unsupported, name)
		}
	}

	for _, candidateURL := range candidateURLs {
		primary, err := resolvePrimaryURL(ctx, fetch, candidateURL, job)
		if err != nil {
			return nil, err
		}
		if primary != nil {
			primary.Evidence = map[string]any{
				"method":    "extracted-url",
				"sourceUrl": job.URL,
			}
			primary.Confidence = 0.9
			return primary, nil
		}
		addUnsupported(classifyUnsupportedURL(candidateURL))
	}

	boardMatch, err := searchPrimaryBoards(ctx, fetch, job)
	if err != nil {
		return nil, err
	}
	if boardMatch != nil {
		return resolution("primary", Resolution{
			Platform:   boardMatch.Platform,
			URL:        boardMatch.URL,
			Job:        boardMatch,
			Confidence: 0.82,
			Evidence: map[string]any{
				"method":    "company-title-board-search",
				"sourceUrl": job.URL,
			},
		}), nil
	}

	geminiMatch, err := resolveViaGemini(ctx, fetch, options.Gemini, job)
	if err != nil {
		return nil, err
	}
	if geminiMatch != nil && geminiMatch.Status == "primary" {
		return geminiMatch, nil
	}

	addUnsupported(classifyUnsupportedURL(job.URL))
	if len(unsupported) > 0 {
		return resolution("unsupported", Resolution{
			Confidence: 0.75,
			Evidence: map[string]any{
				"unsupportedPlatform": unsupported[0],
				"candidates":          unsupported,
			},
		}), nil
	}

	reason := "source-fetch-failed"
	if page != nil {
		reason = "no-primary-ats-found"
	}
	return resolution("unresolved", Resolution{
		Evidence: map[string]any{
			"reason":         reason,
			"candidateCount": len(candidateURLs),
		},
	}), nil
}
