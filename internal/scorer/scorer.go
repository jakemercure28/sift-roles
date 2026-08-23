package scorer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Job is the minimal job shape the scorer needs (a subset of the jobs row).
type Job struct {
	Title       string
	Company     string
	Location    string
	Description string
	// ExtractFacts, when non-empty, is the job's cached career-agnostic fact
	// sheet from global_job_extracts (JSON). The prompt then presents the facts
	// in place of the full description, cutting the per-job payload roughly
	// tenfold; empty falls back to Description.
	ExtractFacts string
}

// Result is a parsed score. Score is nil when the model output could not be
// parsed into a 1-10 integer; Reasoning then carries the diagnostic text.
type Result struct {
	Score     *int
	Reasoning string
}

// Scorer scores jobs against the candidate's resume.md, which is read from
// dataDir (DATA_DIR) on first use and cached until Reload.
type Scorer struct {
	client  *Client
	dataDir string

	// rescoreThreshold turns on two-stage scoring inside ScoreJobs: a job whose
	// batch score lands at or above this value is re-scored individually so a
	// crowded batch's inflated high scores get a full-attention second look. 0
	// disables the second pass (the plain batch+reconcile behavior).
	rescoreThreshold int

	// profile caches the loaded resume plus the experience-years estimate
	// derived from it (every prompt states that figure, so the full-resume
	// regex pass must not repeat). Atomic so Reload can drop the cache while
	// scoring is in flight: the dashboard's Scorer is process-lived, and before
	// Reload existed a resume upload kept scoring against the OLD resume until
	// restart. nil means not loaded yet.
	profile atomic.Pointer[resumeProfile]
}

// resumeProfile is the immutable snapshot loadProfile caches.
type resumeProfile struct {
	resume string
	years  float64
	err    error
}

// New builds a Scorer that reads resume.md from dataDir.
func New(client *Client, dataDir string) *Scorer {
	return &Scorer{client: client, dataDir: dataDir}
}

// HashResume returns the canonical hash of a resume's content, the value
// stamped into jobs.resume_hash at score time. Trims like the scorer's loader
// so the dashboard (hashing the just-saved file content) and the pipeline
// (hashing the loaded resume) always agree.
func HashResume(content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(sum[:])
}

// ResumeHash reports the hash of the loaded resume (loading it on first use).
func (s *Scorer) ResumeHash() (string, error) {
	p := s.loadProfile()
	if p.err != nil {
		return "", p.err
	}
	return HashResume(p.resume), nil
}

// WithRescoreThreshold enables two-stage scoring: batch results at or above n are
// re-scored individually. n <= 0 disables it. Returns the scorer for chaining.
func (s *Scorer) WithRescoreThreshold(n int) *Scorer {
	s.rescoreThreshold = n
	return s
}

// loadProfile returns the cached resume snapshot, reading resume.md on first
// use (or after Reload). A concurrent double-load is harmless: both goroutines
// read the same file and the last Store wins with equivalent content.
func (s *Scorer) loadProfile() *resumeProfile {
	if p := s.profile.Load(); p != nil {
		return p
	}
	p := &resumeProfile{}
	p.resume, p.err = readProfileFile(s.dataDir, "resume.md")
	if p.err == nil {
		p.years = estimateCandidateExperienceYears(p.resume, time.Now())
	}
	s.profile.Store(p)
	return p
}

// Reload drops the cached resume so the next call re-reads resume.md. Called
// after a resume upload; without it a process-lived Scorer (the dashboard's)
// keeps scoring against the old resume until restart.
func (s *Scorer) Reload() {
	s.profile.Store(nil)
}

func readProfileFile(dir, name string) (string, error) {
	path := filepath.Join(dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w (expected at %s)", name, err, path)
	}
	return strings.TrimSpace(string(data)), nil
}

// ScorePrompt returns the exact prompt the sync path sends for job, for
// callers that route the model call elsewhere (the Batch API). Keeping batch
// submissions on this builder means a batch-scored job sees the identical
// rubric, resume, and job block as a sync-scored one.
func (s *Scorer) ScorePrompt(job Job) (string, error) {
	p := s.loadProfile()
	if p.err != nil {
		return "", p.err
	}
	return buildScorePrompt(p.resume, p.years, job), nil
}

// ParseResult runs a raw model response through the same parse pipeline as the
// sync path, so batch results land with identical semantics.
func (s *Scorer) ParseResult(_ Job, text string) Result {
	return parseScoreResponse(text)
}

// ScoreJob scores one job 1-10. It loads the profile lazily on first use.
func (s *Scorer) ScoreJob(ctx context.Context, job Job) (Result, error) {
	p := s.loadProfile()
	if p.err != nil {
		return Result{}, p.err
	}
	prompt := buildScorePrompt(p.resume, p.years, job)
	text, err := s.client.CallGeminiScore(ctx, prompt, scoreMaxOutputToken)
	if err != nil {
		return Result{}, err
	}
	return parseScoreResponse(text), nil
}

// scoreRubric is the grounding/disqualifier/scoring rules shared verbatim by the
// single-job and batch prompts so the two never drift.
const scoreRubric = `Be skeptical and realistic: a high score means this resume would likely pass a recruiter or hiring-manager screen for THIS specific role, ahead of the typical applicant pool, not merely that the candidate could do or learn the job. Score the screen-pass likelihood, not raw capability. Work through these steps in order before you score. This applies to ANY profession — software, medicine, finance, design, trades, or anything else — so judge by the role's own requirements, not by any assumed industry.

STEP 1 — EXTRACT the role's: field/function; the core day-to-day work the role is actually hired to do; required credentials, licenses, or certifications; required skills/tools; and the highest stated minimum years of experience.

STEP 2 — CHECK each literally against the resume. A credential, skill, function, or number of years counts ONLY if the resume actually shows it. Never assume, infer, or invent experience the candidate has not written down, and never claim the candidate has already done work or used a tool that is not in the resume.

STEP 3 — HARD CAPS (score 1–4), no matter how strong the keyword overlap: (1) the role is a fundamentally different FIELD or FUNCTION than the candidate's background; or (2) the role requires a credential, license, or certification the resume does not show (e.g. a medical board certification, bar admission, CPA, PE license, security clearance). Years of experience are NEVER a hard cap: see STEP 3a.

STEP 3a — YEARS OF EXPERIENCE ARE A SOFT SIGNAL, NOT A REQUIREMENT. A stated minimum ("4+ years", "5-7 years", "minimum 4 YOE") is the employer's aspiration, not a filter, and real screens routinely advance candidates below it when the function matches. Never disqualify, never hard-cap, and never drop a score into the 1–4 band because of a years shortfall. A shortfall of up to about two years is normal and costs at most a point off an otherwise strong score. Even a larger gap is only one factor weighed against field fit, core function, and demonstrated skills. When a range is given ("3-5 years"), treat the LOW end as the number that matters. Do not do your own date arithmetic on the resume: the candidate's total experience is stated for you below, so use that figure and nothing else.

STEP 4 — LEARNABLE TOOLS ARE NOT HARD CAPS, BUT A FUNCTION MISMATCH STILL LOWERS THE SCORE. A missing tool, framework, or programming language is a growth area, not a hard cap: when the role's core function matches what the resume already does, assume the candidate can pick it up and do not drop the score for it (you still may not pretend the candidate already HAS it). BUT when the role's CORE DAY-TO-DAY FUNCTION differs from the function the resume demonstrates, that is a weak match even inside the same broad field: in a real screen, specialists whose experience directly matches the function will outrank this candidate. Do NOT let "it's learnable" push such a role into the strong-fit band; score it in the middle. Reserve high scores for roles whose core function the resume actually demonstrates.

FORBIDDEN — Do not use "transferable", "adjacent", or "pivot" to wave away a different FIELD/FUNCTION or a missing required CREDENTIAL — those are hard caps. (Crediting the candidate with picking up a missing tool, framework, or language when the function already matches, per Step 4, is expected and fine.)

STEP 5 — LOCATION & AUTHORIZATION (hard cap 1–4): when the candidate has STATED a location, work-authorization, relocation, or timezone constraint, treat a role that plainly violates it as a hard cap, even with perfect skill overlap (e.g. an on-site or region-locked role the candidate cannot take, named in the title or location). Apply this only to a constraint the candidate actually wrote; never invent one. A role open to the candidate's stated region, or open to remote when the candidate accepts remote, is fine.

STEP 6 — SCORE only when no hard cap fired, as the likelihood this resume passes the screen for this role relative to typical applicants. Weigh field fit, whether the resume shows the role's core function, seniority, how many of the role's required skills the resume already shows, and any stated preferences or dealbreakers in the resume. A role whose field AND core function the resume directly demonstrates scores high even when a few learnable tools are missing, and stays high when the candidate is somewhat under the stated years; a role in the candidate's field but built around a different core function scores in the middle, not high.

NAMING — In the reasoning, refer to the candidate only as the resume does. Never invent a name, gender, or pronoun the resume does not state; if unsure, write "the candidate".`

// formatJobBlock renders one job's fields with the same defaults the single-job
// prompt has always used. When the job carries cached extract facts they stand
// in for the raw description: the facts JSON is the rubric's STEP 1 already
// done, so the block says so and the model proceeds from STEP 2.
func formatJobBlock(job Job) string {
	location := job.Location
	if location == "" {
		location = "Not specified"
	}
	head := "**Title:** " + job.Title + "\n" +
		"**Company:** " + job.Company + "\n" +
		"**Location:** " + location + "\n\n"
	if job.ExtractFacts != "" {
		return head + "**Extracted role facts (structured extraction of the full listing; treat as STEP 1's result):**\n" + job.ExtractFacts
	}
	description := job.Description
	if description == "" {
		description = "No description available."
	}
	// Tighter than ScoreRejection's 15000: the first ~4k chars of a JD carry
	// the role, requirements, and qualifications; the tail is benefits/EEO
	// boilerplate that only inflates the per-job prompt spend.
	if len(description) > scoreDescriptionLength {
		description = description[:scoreDescriptionLength]
	}
	return head + "**Description:**\n" + description
}

// scoreDescriptionLength caps the JD text sent to the scoring prompts.
const scoreDescriptionLength = 4000

// Block order in both prompts is deliberate: the fully static intro + rubric +
// response-format prefix leads (identical across every tenant and call), the
// per-tenant resume follows, and the per-call job block(s) come last. Gemini's
// implicit prompt caching discounts repeated request prefixes, so the
// most-stable content must lead; a resume-first prompt busts the shared prefix
// at byte one of every call.
// resumeBlock renders the resume plus the deterministic experience-years figure
// derived from its dated history. The number is stated for the model because it
// otherwise recomputes the span per job and disagrees with itself across jobs
// scored the same day (one listing got "4 years", another "3 years and 10
// months", from this same resume). years <= 0 means the resume had no datable
// history, so nothing is asserted.
func resumeBlock(resume string, years float64) string {
	if years <= 0 {
		return resume
	}
	return resume + fmt.Sprintf(
		"\n\n**Total professional experience (computed from the dated history above, authoritative):** about %.1f years. Use this figure for any years comparison. Do not recompute it, and do not contradict it.",
		years,
	)
}

func buildScorePrompt(resume string, years float64, job Job) string {
	return `You are evaluating how well a job listing matches a candidate.

Score the job listing at the end 1–10 for how likely this resume is to pass a recruiter or hiring-manager screen for it, relative to the typical applicant pool. ` + scoreRubric + `

Respond in EXACTLY this format (no other text):
SCORE: <integer 1-10>
REASONING: <if the score is 5 or higher: 2-4 sentences naming the role's core requirements, whether the resume actually shows each one, then the verdict. If the score is 4 or lower: ONE sentence of at most 20 words naming the single biggest mismatch. Do not claim experience that is not written in the resume.>

## Candidate Resume
` + resumeBlock(resume, years) + `

---

## Job Listing
` + formatJobBlock(job)
}

// batchJobKey labels job i in the batch prompt; parseBatchResponse inverts it.
// The model echoes the label printed on each job block instead of counting
// positions, so index drift misattributing one job's score to another becomes
// a dropped entry that reconciliation re-scores, never a silent swap.
func batchJobKey(i int) string { return "j" + strconv.Itoa(i) }

// buildBatchPrompt asks the model to score a keyed list of jobs and return a
// JSON array; the rubric is identical to the single-job prompt, and the block
// order follows the same static-prefix-first rule (see buildScorePrompt).
func buildBatchPrompt(resume string, years float64, jobs []Job) string {
	var b strings.Builder
	b.WriteString("You are evaluating how well several job listings match a candidate.\n\n")
	b.WriteString("Evaluate each job listing in COMPLETE ISOLATION, as if it were the only job in this request. One job's content, quality, or score must have ZERO influence on any other job's score: do not compare the jobs to each other, do not rank or curve them against each other, and do not let a strong or weak neighboring job shift a score up or down. Write each job's reasoning from scratch, grounded only in that job and the resume; do not reuse or adapt phrasing from another job's reasoning.\n\n")
	b.WriteString("Score EACH job 1–10 for how likely this resume is to pass a recruiter or hiring-manager screen for it, relative to the typical applicant pool. ")
	b.WriteString(scoreRubric)
	b.WriteString(`

Return a JSON array with one object per job. Each object has:
- "job_id": the job's id from its "### Job <job_id>" heading, copied EXACTLY (string, e.g. "j0")
- "score": integer 1-10
- "reasoning": if the score is 5 or higher, 2-4 sentences naming the role's core requirements, whether the resume shows each one, then the verdict; if the score is 4 or lower, ONE sentence of at most 20 words naming the single biggest mismatch. Do not claim experience not written in the resume.
Include every job exactly once.

## Candidate Resume
`)
	b.WriteString(resumeBlock(resume, years))
	b.WriteString(`

---

## Job Listings`)
	for i, job := range jobs {
		fmt.Fprintf(&b, "\n\n### Job %s\n", batchJobKey(i))
		b.WriteString(formatJobBlock(job))
	}
	return b.String()
}

// batchSchema constrains batch output to a JSON array of {job_id, score,
// reasoning}. Gemini's responseSchema uses uppercase OpenAPI type names.
var batchSchema = json.RawMessage(`{
  "type": "ARRAY",
  "items": {
    "type": "OBJECT",
    "properties": {
      "job_id": {"type": "STRING"},
      "score": {"type": "INTEGER"},
      "reasoning": {"type": "STRING"}
    },
    "required": ["job_id", "score", "reasoning"]
  }
}`)

const (
	batchBaseTokens   = 256
	batchTokensPerJob = 150
)

// ScoreJobs scores a batch of jobs in a single Gemini call, returning results in
// the same order as jobs. A job the batch response omits or returns unparsably is
// re-scored individually (reconciliation) so batching never silently drops a job.
// A hard call error (network/quota) fails the whole batch and is returned as-is.
func (s *Scorer) ScoreJobs(ctx context.Context, jobs []Job) ([]Result, error) {
	if p := s.loadProfile(); p.err != nil {
		return nil, p.err
	}
	if len(jobs) == 0 {
		return nil, nil
	}
	if len(jobs) == 1 {
		r, err := s.ScoreJob(ctx, jobs[0])
		if err != nil {
			return nil, err
		}
		return []Result{r}, nil
	}

	parsed, err := s.scoreBatch(ctx, jobs)
	if err != nil {
		return nil, err
	}

	results := make([]Result, len(jobs))
	// survivors collects batch-scored jobs that landed high enough to warrant a
	// full-attention single-job re-score (stage two). Reconciled jobs already went
	// through ScoreJob, so they are never added here.
	var survivors []int
	for i := range jobs {
		if r, ok := parsed[i]; ok {
			results[i] = r
			if s.rescoreThreshold > 0 && results[i].Score != nil && *results[i].Score >= s.rescoreThreshold {
				survivors = append(survivors, i)
			}
			continue
		}
		// Reconciliation: the batch dropped this job; re-score it on its own.
		r, rerr := s.ScoreJob(ctx, jobs[i])
		if rerr != nil {
			results[i] = Result{Reasoning: "batch omitted job and reconciliation failed: " + rerr.Error()}
			continue
		}
		results[i] = r
	}
	// Stage two: re-score the high-scoring survivors individually. A crowded batch
	// inflates strong-fit scores; a single-job pass gives the model full attention
	// on the few jobs that actually surface to the user, catching false positives.
	// On a re-score error keep the batch score rather than dropping the result.
	for _, i := range survivors {
		if r, err := s.ScoreJob(ctx, jobs[i]); err == nil {
			results[i] = r
		}
	}
	return results, nil
}

func (s *Scorer) scoreBatch(ctx context.Context, jobs []Job) (map[int]Result, error) {
	p := s.loadProfile()
	prompt := buildBatchPrompt(p.resume, p.years, jobs)
	maxTokens := batchBaseTokens + len(jobs)*batchTokensPerJob
	text, err := s.client.CallGeminiJSON(ctx, prompt, maxTokens, batchSchema)
	if err != nil {
		return nil, err
	}
	return parseBatchResponse(text, len(jobs)), nil
}

// parseBatchResponse maps a JSON batch response back to slice positions via
// the batch's j<N> job_id labels (see batchJobKey). Unknown-format,
// out-of-range, duplicate, or unparsable entries are skipped so the caller can
// reconcile whatever is missing.
func parseBatchResponse(text string, n int) map[int]Result {
	out := make(map[int]Result)
	var items []struct {
		JobID     string `json:"job_id"`
		Score     int    `json:"score"`
		Reasoning string `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(stripJSONFences(text)), &items); err != nil {
		return out
	}
	for _, it := range items {
		idx, ok := strings.CutPrefix(strings.TrimSpace(it.JobID), "j")
		if !ok {
			continue
		}
		i, err := strconv.Atoi(idx)
		if err != nil || i < 0 || i >= n {
			continue
		}
		if _, dup := out[i]; dup {
			continue
		}
		score := clampScore(it.Score)
		out[i] = Result{Score: &score, Reasoning: strings.TrimSpace(it.Reasoning)}
	}
	return out
}

// stripJSONFences tolerates a model that wraps JSON in a ```json code fence even
// though structured output should return raw JSON.
func stripJSONFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(s, "```")
	}
	return strings.TrimSpace(s)
}

func clampScore(n int) int {
	if n < 1 {
		return 1
	}
	if n > 10 {
		return 10
	}
	return n
}

var (
	scoreRe     = regexp.MustCompile(`(?m)^SCORE:\s*(\d+)`)
	reasoningRe = regexp.MustCompile(`(?ms)^REASONING:\s*(.+)`)
)

// parseScoreResponse extracts the score and reasoning, mirroring scorer.js:
// the score is clamped to 1-10; an unparsable score yields a nil Score and a
// diagnostic Reasoning.
func parseScoreResponse(text string) Result {
	var score *int
	if m := scoreRe.FindStringSubmatch(text); m != nil {
		n, _ := strconv.Atoi(m[1])
		n = clampScore(n)
		score = &n
	}

	var reasoning string
	switch {
	case reasoningRe.MatchString(text):
		reasoning = strings.TrimSpace(reasoningRe.FindStringSubmatch(text)[1])
	case score != nil:
		reasoning = text
	default:
		raw := text
		if len(raw) > 200 {
			raw = raw[:200]
		}
		reasoning = "Score parse failed. Raw: " + raw
	}

	return Result{Score: score, Reasoning: reasoning}
}

type experienceInterval struct {
	start time.Time
	end   time.Time
}

var (
	explicitResumeYearsRe = regexp.MustCompile(`(?i)\b(\d{1,2})\+?\s*(?:years?|yrs?)\s+(?:of\s+)?(?:professional\s+|industry\s+|relevant\s+|software\s+|engineering\s+|devops\s+|sre\s+|platform\s+|infrastructure\s+|technical\s+|work\s+)*experience\b`)
	monthRangeRe          = regexp.MustCompile(`(?i)\b(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\.?\s+(\d{4})\s*(?:-|–|—|to)\s*(present|current|now|jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\.?\s*(\d{4})?`)
	yearRangeRe           = regexp.MustCompile(`(?i)\b((?:19|20)\d{2})\s*(?:-|–|—|to)\s*(present|current|now|(?:19|20)\d{2})\b`)
)

var educationContextWords = []string{
	"education", "degree", "university", "college", "school", "certification", "certified", "bachelor", "master", "phd", "b.s.", "m.s.",
}

// estimateCandidateExperienceYears returns the larger of (a) an explicit "N years
// of experience" summary in the resume and (b) the merged span of dated work
// intervals, so overlapping or back-to-back roles are not double-counted.
func estimateCandidateExperienceYears(resume string, now time.Time) float64 {
	now = monthStart(now)
	years := float64(maxExplicitResumeExperienceYears(resume))
	intervals := resumeExperienceIntervals(resume, now)
	if len(intervals) == 0 {
		return years
	}
	sort.Slice(intervals, func(i, j int) bool {
		if intervals[i].start.Equal(intervals[j].start) {
			return intervals[i].end.Before(intervals[j].end)
		}
		return intervals[i].start.Before(intervals[j].start)
	})

	merged := intervals[:0]
	for _, iv := range intervals {
		if !iv.end.After(iv.start) {
			continue
		}
		if len(merged) == 0 || iv.start.After(merged[len(merged)-1].end) {
			merged = append(merged, iv)
			continue
		}
		if iv.end.After(merged[len(merged)-1].end) {
			merged[len(merged)-1].end = iv.end
		}
	}

	months := 0
	for _, iv := range merged {
		months += monthDiff(iv.start, iv.end)
	}
	rangeYears := float64(months) / 12.0
	if rangeYears > years {
		return rangeYears
	}
	return years
}

func maxExplicitResumeExperienceYears(resume string) int {
	maxYears := 0
	for _, m := range explicitResumeYearsRe.FindAllStringSubmatch(resume, -1) {
		if len(m) < 2 {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err == nil && n > maxYears {
			maxYears = n
		}
	}
	return maxYears
}

// resumeExperienceIntervals collects dated work spans from the resume, skipping
// ranges that sit on an education/certification line so a degree's "2008–2012"
// is not counted as work experience.
func resumeExperienceIntervals(resume string, now time.Time) []experienceInterval {
	var intervals []experienceInterval
	masked := []byte(resume)
	for _, loc := range monthRangeRe.FindAllStringSubmatchIndex(resume, -1) {
		if len(loc) < 10 {
			continue
		}
		if hasAnyFold(lineAt(resume, loc[0]), educationContextWords) {
			continue
		}
		m := []string{
			resume[loc[0]:loc[1]],
			resume[loc[2]:loc[3]],
			resume[loc[4]:loc[5]],
			resume[loc[6]:loc[7]],
			"",
		}
		if loc[8] >= 0 && loc[9] >= 0 {
			m[4] = resume[loc[8]:loc[9]]
		}
		startMonth, ok := parseMonth(m[1])
		if !ok {
			continue
		}
		startYear, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		end := now
		if !isPresentDate(m[3]) {
			endMonth, ok := parseMonth(m[3])
			if !ok || m[4] == "" {
				continue
			}
			endYear, err := strconv.Atoi(m[4])
			if err != nil {
				continue
			}
			end = time.Date(endYear, endMonth, 1, 0, 0, 0, 0, time.UTC)
		}
		intervals = append(intervals, experienceInterval{
			start: time.Date(startYear, startMonth, 1, 0, 0, 0, 0, time.UTC),
			end:   monthStart(end),
		})
		for i := loc[0]; i < loc[1]; i++ {
			masked[i] = ' '
		}
	}
	maskedResume := string(masked)
	for _, loc := range yearRangeRe.FindAllStringSubmatchIndex(maskedResume, -1) {
		if len(loc) < 6 {
			continue
		}
		if hasAnyFold(lineAt(resume, loc[0]), educationContextWords) {
			continue
		}
		m := []string{
			maskedResume[loc[0]:loc[1]],
			maskedResume[loc[2]:loc[3]],
			maskedResume[loc[4]:loc[5]],
		}
		startYear, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		end := now
		if !isPresentDate(m[2]) {
			endYear, err := strconv.Atoi(m[2])
			if err != nil {
				continue
			}
			end = time.Date(endYear, time.January, 1, 0, 0, 0, 0, time.UTC)
		}
		intervals = append(intervals, experienceInterval{
			start: time.Date(startYear, time.January, 1, 0, 0, 0, 0, time.UTC),
			end:   monthStart(end),
		})
	}
	return intervals
}

func parseMonth(raw string) (time.Month, bool) {
	s := strings.ToLower(strings.Trim(raw, ". "))
	if len(s) < 3 {
		return time.January, false
	}
	switch s[:3] {
	case "jan":
		return time.January, true
	case "feb":
		return time.February, true
	case "mar":
		return time.March, true
	case "apr":
		return time.April, true
	case "may":
		return time.May, true
	case "jun":
		return time.June, true
	case "jul":
		return time.July, true
	case "aug":
		return time.August, true
	case "sep":
		return time.September, true
	case "oct":
		return time.October, true
	case "nov":
		return time.November, true
	case "dec":
		return time.December, true
	default:
		return time.January, false
	}
}

func isPresentDate(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "present", "current", "now":
		return true
	default:
		return false
	}
}

func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func monthDiff(start, end time.Time) int {
	return (end.Year()-start.Year())*12 + int(end.Month()-start.Month())
}

func lineAt(text string, offset int) string {
	if offset < 0 {
		offset = 0
	}
	if offset > len(text) {
		offset = len(text)
	}
	start := strings.LastIndex(text[:offset], "\n") + 1
	endRel := strings.Index(text[offset:], "\n")
	if endRel < 0 {
		return text[start:]
	}
	return text[start : offset+endRel]
}

func hasAnyFold(text string, needles []string) bool {
	lower := strings.ToLower(text)
	for _, needle := range needles {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}
