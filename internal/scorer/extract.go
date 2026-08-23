package scorer

import (
	"context"
	"encoding/json"
	"strings"
)

// This file owns extract-once: one LLM call per GLOBAL job that materializes
// the scoring rubric's STEP 1 (the role's field/function, core work,
// credentials, skills, min years) into a career-agnostic facts JSON stored in
// global_job_extracts. The extract carries NO fit judgment and never sees a
// resume; per-career pass/fail gates were tried and rejected, so all judgment
// stays in the per-tenant scoring call that later consumes these facts.

// ExtractVersion invalidates stored extracts in bulk when the prompt or schema
// below changes shape. Bump it on any change that alters what facts mean.
const ExtractVersion = 1

// extractMaxOutputTokens bounds one extraction response. The facts are short
// structured fields plus a 2-3 sentence summary.
const extractMaxOutputTokens = 512

// ExtractFacts is the career-agnostic fact sheet for one job listing.
type ExtractFacts struct {
	FieldFunction       string   `json:"field_function"`
	CoreWork            string   `json:"core_work"`
	RequiredCredentials []string `json:"required_credentials"`
	RequiredSkills      []string `json:"required_skills"`
	MinYears            int      `json:"min_years"`
	Seniority           string   `json:"seniority"`
	LocationPolicy      string   `json:"location_policy"`
	Compensation        string   `json:"compensation"`
	Summary             string   `json:"summary"`
}

// extractSchema constrains extraction output to the ExtractFacts shape.
// Gemini's responseSchema uses uppercase OpenAPI type names.
var extractSchema = json.RawMessage(`{
  "type": "OBJECT",
  "properties": {
    "field_function": {"type": "STRING"},
    "core_work": {"type": "STRING"},
    "required_credentials": {"type": "ARRAY", "items": {"type": "STRING"}},
    "required_skills": {"type": "ARRAY", "items": {"type": "STRING"}},
    "min_years": {"type": "INTEGER"},
    "seniority": {"type": "STRING"},
    "location_policy": {"type": "STRING"},
    "compensation": {"type": "STRING"},
    "summary": {"type": "STRING"}
  },
  "required": ["field_function", "core_work", "required_credentials", "required_skills", "min_years", "seniority", "location_policy", "compensation", "summary"]
}`)

// buildExtractPrompt renders the extraction request for one job. The prompt is
// fully static apart from the trailing job block (the same prefix-caching rule
// the scoring prompts follow), names no profession, and forbids inference so
// the stored facts stay grounded in the listing text.
func buildExtractPrompt(job Job) string {
	return `You are extracting structured facts from a job listing. Report ONLY what the listing itself states; never infer, assume, or add information that is not written in it. This applies to ANY profession — software, medicine, finance, design, trades, or anything else — so use the listing's own words for its field and requirements.

Extract:
- field_function: the professional field and core function of the role, in a few words.
- core_work: 1-2 sentences describing the core day-to-day work the role is actually hired to do.
- required_credentials: credentials, licenses, or certifications the listing REQUIRES (empty list if none are stated).
- required_skills: the skills and tools the listing requires or strongly emphasizes.
- min_years: the highest stated minimum years of experience (0 if none is stated).
- seniority: the level the listing states or its title implies (e.g. junior, mid, senior, staff, lead, manager, director; empty string if unclear).
- location_policy: the work arrangement and region exactly as stated (e.g. "remote, US only", "hybrid, London", "on-site, Seattle WA"; empty string if not stated).
- compensation: the stated pay or range, verbatim (empty string if not stated).
- summary: 2-3 sentences capturing anything important about the role the fields above miss (team, domain, unusual requirements or constraints).

## Job Listing
` + formatJobBlock(job)
}

// Extractor runs extract-once calls. Unlike Scorer it holds no resume; the
// extract must stay tenant-independent so it can be shared globally.
type Extractor struct {
	client *Client
}

// NewExtractor builds an Extractor on an existing Gemini client (same model,
// pacing, and usage accounting as scoring).
func NewExtractor(client *Client) *Extractor {
	return &Extractor{client: client}
}

// ExtractJob extracts one job's facts and returns them as the canonical JSON
// string stored in global_job_extracts. The parse round-trip validates shape
// and normalizes field order so stored facts are stable across calls.
func (e *Extractor) ExtractJob(ctx context.Context, job Job) (string, error) {
	text, err := e.client.CallGeminiExtract(ctx, buildExtractPrompt(job), extractMaxOutputTokens, extractSchema)
	if err != nil {
		return "", err
	}
	facts, err := parseExtractResponse(text)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(facts)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// parseExtractResponse decodes the model's JSON, tolerating code fences the
// same way batch scoring does.
func parseExtractResponse(text string) (ExtractFacts, error) {
	trimmed := strings.TrimSpace(text)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	var facts ExtractFacts
	if err := json.Unmarshal([]byte(strings.TrimSpace(trimmed)), &facts); err != nil {
		return ExtractFacts{}, err
	}
	if facts.RequiredCredentials == nil {
		facts.RequiredCredentials = []string{}
	}
	if facts.RequiredSkills == nil {
		facts.RequiredSkills = []string{}
	}
	return facts, nil
}
