package scorer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Job is the minimal job shape the scorer needs (a subset of the jobs row).
type Job struct {
	Title       string
	Company     string
	Location    string
	Description string
}

// Result is a parsed score. Score is nil when the model output could not be
// parsed into a 1-10 integer; Reasoning then carries the diagnostic text.
type Result struct {
	Score     *int
	Reasoning string
}

// Scorer scores jobs against the candidate's resume.md, which is read once from
// dataDir (DATA_DIR) and cached.
type Scorer struct {
	client  *Client
	dataDir string

	// rescoreThreshold turns on two-stage scoring inside ScoreJobs: a job whose
	// batch score lands at or above this value is re-scored individually so a
	// crowded batch's inflated high scores get a full-attention second look. 0
	// disables the second pass (the plain batch+reconcile behavior).
	rescoreThreshold int

	once    sync.Once
	resume  string
	loadErr error
}

// New builds a Scorer that reads resume.md from dataDir.
func New(client *Client, dataDir string) *Scorer {
	return &Scorer{client: client, dataDir: dataDir}
}

// WithRescoreThreshold enables two-stage scoring: batch results at or above n are
// re-scored individually. n <= 0 disables it. Returns the scorer for chaining.
func (s *Scorer) WithRescoreThreshold(n int) *Scorer {
	s.rescoreThreshold = n
	return s
}

func (s *Scorer) load() {
	resume, err := readProfileFile(s.dataDir, "resume.md")
	if err != nil {
		s.loadErr = err
		return
	}
	s.resume = resume
}

func readProfileFile(dir, name string) (string, error) {
	path := filepath.Join(dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w (expected at %s)", name, err, path)
	}
	return strings.TrimSpace(string(data)), nil
}

// ScoreJob scores one job 1-10. It loads the profile lazily on first use.
func (s *Scorer) ScoreJob(ctx context.Context, job Job) (Result, error) {
	s.once.Do(s.load)
	if s.loadErr != nil {
		return Result{}, s.loadErr
	}
	prompt := buildScorePrompt(s.resume, job)
	text, err := s.client.CallGeminiScore(ctx, prompt, defaultMaxOutputToken)
	if err != nil {
		return Result{}, err
	}
	return calibrateScore(job, s.resume, parseScoreResponse(text)), nil
}

// scoreRubric is the grounding/disqualifier/scoring rules shared verbatim by the
// single-job and batch prompts so the two never drift.
const scoreRubric = `Be skeptical and realistic: a high score means this resume would likely pass a recruiter or hiring-manager screen for THIS specific role, ahead of the typical applicant pool, not merely that the candidate could do or learn the job. Score the screen-pass likelihood, not raw capability. Work through these steps in order before you score. This applies to ANY profession — software, medicine, finance, design, trades, or anything else — so judge by the role's own requirements, not by any assumed industry.

STEP 1 — EXTRACT the role's: field/function; the core day-to-day work the role is actually hired to do; required credentials, licenses, or certifications; required skills/tools; and the highest stated minimum years of experience.

STEP 2 — CHECK each literally against the resume. A credential, skill, function, or number of years counts ONLY if the resume actually shows it. Never assume, infer, or invent experience the candidate has not written down, and never claim the candidate has already done work or used a tool that is not in the resume.

STEP 3 — HARD CAPS (score 1–4), no matter how strong the keyword overlap: (1) the role is a fundamentally different FIELD or FUNCTION than the candidate's background; (2) the role requires a credential, license, or certification the resume does not show (e.g. a medical board certification, bar admission, CPA, PE license, security clearance); or (3) the resume is more than about one year below the role's stated minimum years of experience.

STEP 4 — LEARNABLE TOOLS ARE NOT HARD CAPS, BUT A FUNCTION MISMATCH STILL LOWERS THE SCORE. A missing tool, framework, or programming language is a growth area, not a hard cap: when the role's core function matches what the resume already does, assume the candidate can pick it up and do not drop the score for it (you still may not pretend the candidate already HAS it). BUT when the role's CORE DAY-TO-DAY FUNCTION differs from the function the resume demonstrates, that is a weak match even inside the same broad field: in a real screen, specialists whose experience directly matches the function will outrank this candidate. Do NOT let "it's learnable" push such a role into the strong-fit band; score it in the middle. Reserve high scores for roles whose core function the resume actually demonstrates.

FORBIDDEN — Do not use "transferable", "adjacent", or "pivot" to wave away a different FIELD/FUNCTION or a missing required CREDENTIAL — those are hard caps. (Crediting the candidate with picking up a missing tool, framework, or language when the function already matches, per Step 4, is expected and fine.)

STEP 5 — LOCATION & AUTHORIZATION (hard cap 1–4): when the candidate has STATED a location, work-authorization, relocation, or timezone constraint, treat a role that plainly violates it as a hard cap, even with perfect skill overlap (e.g. an on-site or region-locked role the candidate cannot take, named in the title or location). Apply this only to a constraint the candidate actually wrote; never invent one. A role open to the candidate's stated region, or open to remote when the candidate accepts remote, is fine.

STEP 6 — SCORE only when no hard cap fired, as the likelihood this resume passes the screen for this role relative to typical applicants. Weigh field fit, whether the resume shows the role's core function, seniority, years, how many of the role's required skills the resume already shows, and any stated preferences or dealbreakers in the resume. A role whose field AND core function the resume directly demonstrates scores high even when a few learnable tools are missing; a role in the candidate's field but built around a different core function scores in the middle, not high; a one-year shortfall on minimum years is fine, a multi-year shortfall is not.

NAMING — In the reasoning, refer to the candidate only as the resume does. Never invent a name, gender, or pronoun the resume does not state; if unsure, write "the candidate".`

// formatJobBlock renders one job's fields with the same defaults the single-job
// prompt has always used.
func formatJobBlock(job Job) string {
	location := job.Location
	if location == "" {
		location = "Not specified"
	}
	description := job.Description
	if description == "" {
		description = "No description available."
	}
	return "**Title:** " + job.Title + "\n" +
		"**Company:** " + job.Company + "\n" +
		"**Location:** " + location + "\n\n" +
		"**Description:**\n" + description
}

func buildScorePrompt(resume string, job Job) string {
	return `You are evaluating how well a job listing matches a candidate.

## Candidate Resume
` + resume + `

---

Score the job listing at the end 1–10 for how likely this resume is to pass a recruiter or hiring-manager screen for it, relative to the typical applicant pool. ` + scoreRubric + `

Respond in EXACTLY this format (no other text):
SCORE: <integer 1-10>
REASONING: <2-4 sentences: name the role's core requirements, state whether the resume actually shows each one, then the verdict. Do not claim experience that is not written in the resume.>

## Job Listing
` + formatJobBlock(job)
}

// buildBatchPrompt asks the model to score an indexed list of jobs and return a
// JSON array; the rubric is identical to the single-job prompt.
func buildBatchPrompt(resume string, jobs []Job) string {
	var b strings.Builder
	b.WriteString("You are evaluating how well several job listings match a candidate.\n\n")
	b.WriteString("## Candidate Resume\n")
	b.WriteString(resume)
	b.WriteString("\n---\n\nScore EACH job 1–10 for how likely this resume is to pass a recruiter or hiring-manager screen for it, relative to the typical applicant pool. ")
	b.WriteString(scoreRubric)
	b.WriteString(`

Return a JSON array with one object per job. Each object has:
- "index": the Job number shown above (integer)
- "score": integer 1-10
- "reasoning": 2-4 sentences naming the role's core requirements, whether the resume shows each one, then the verdict. Do not claim experience not written in the resume.
Include every job exactly once.

## Job Listings`)
	for i, job := range jobs {
		fmt.Fprintf(&b, "\n\n### Job %d\n", i)
		b.WriteString(formatJobBlock(job))
	}
	return b.String()
}

// batchSchema constrains batch output to a JSON array of {index, score, reasoning}.
// Gemini's responseSchema uses uppercase OpenAPI type names.
var batchSchema = json.RawMessage(`{
  "type": "ARRAY",
  "items": {
    "type": "OBJECT",
    "properties": {
      "index": {"type": "INTEGER"},
      "score": {"type": "INTEGER"},
      "reasoning": {"type": "STRING"}
    },
    "required": ["index", "score", "reasoning"]
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
	s.once.Do(s.load)
	if s.loadErr != nil {
		return nil, s.loadErr
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
			// Apply the same deterministic year-gap cap the single-job path gets,
			// so the model's batch score can't inflate an over-leveled role. The
			// reconciliation branch below goes through ScoreJob, which already
			// calibrates, so it must not be calibrated again here.
			results[i] = calibrateScore(jobs[i], s.resume, r)
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
	prompt := buildBatchPrompt(s.resume, jobs)
	maxTokens := batchBaseTokens + len(jobs)*batchTokensPerJob
	text, err := s.client.CallGeminiJSON(ctx, prompt, maxTokens, batchSchema)
	if err != nil {
		return nil, err
	}
	return parseBatchResponse(text, len(jobs)), nil
}

// parseBatchResponse maps a JSON batch response to results keyed by job index.
// Out-of-range, duplicate, or unparsable entries are skipped so the caller can
// reconcile whatever is missing.
func parseBatchResponse(text string, n int) map[int]Result {
	out := make(map[int]Result)
	var items []struct {
		Index     int    `json:"index"`
		Score     int    `json:"score"`
		Reasoning string `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(stripJSONFences(text)), &items); err != nil {
		return out
	}
	for _, it := range items {
		if it.Index < 0 || it.Index >= n {
			continue
		}
		if _, dup := out[it.Index]; dup {
			continue
		}
		score := clampScore(it.Score)
		out[it.Index] = Result{Score: &score, Reasoning: strings.TrimSpace(it.Reasoning)}
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

// calibrateScore is a deterministic safety net over the model's score: when a JD
// states an explicit minimum years-of-experience and the resume's dated history is
// clearly short of it, it caps an inflated score below the strong-fit band. This
// runs on every score (single and batch) so the guardrail holds even when the
// model ignores the YEARS rule inside a 20-job batch. It only ever lowers a score,
// never raises it, and is a no-op when no explicit minimum or no datable resume
// experience is found.
func calibrateScore(job Job, resume string, result Result) Result {
	if result.Score == nil {
		return result
	}

	requiredYears := maxRequiredExperienceYears(job.Title + "\n" + job.Description)
	if requiredYears == 0 {
		return result
	}
	candidateYears := estimateCandidateExperienceYears(resume, time.Now())
	if candidateYears == 0 {
		return result
	}

	capScore := scoreCapForExperienceGap(requiredYears, candidateYears)
	if capScore == 0 || *result.Score <= capScore {
		return result
	}

	score := capScore
	result.Score = &score
	note := fmt.Sprintf("Calibration: JD requires %d+ years; resume shows about %.0f years, so this is capped below a strong-fit score.", requiredYears, candidateYears)
	if result.Reasoning == "" {
		result.Reasoning = note
	} else if !strings.Contains(result.Reasoning, "Calibration: JD requires") {
		result.Reasoning = strings.TrimSpace(result.Reasoning) + " " + note
	}
	return result
}

// scoreCapForExperienceGap returns the highest score a job may keep given the gap
// between its required years and the candidate's estimated years, or 0 for "no
// cap". A shortfall of about a year or less never caps.
func scoreCapForExperienceGap(requiredYears int, candidateYears float64) int {
	shortfall := float64(requiredYears) - candidateYears
	if shortfall <= 1.0 {
		return 0
	}
	if requiredYears >= 10 {
		if shortfall >= 5 || candidateYears/float64(requiredYears) <= 0.6 {
			return 5
		}
		if shortfall >= 3 {
			return 6
		}
		return 7
	}
	if shortfall >= 4 || candidateYears/float64(requiredYears) <= 0.5 {
		return 5
	}
	if shortfall >= 2 {
		return 7
	}
	return 0
}

type experienceInterval struct {
	start time.Time
	end   time.Time
}

var (
	requiredYearsRes = []*regexp.Regexp{
		// Lead-in phrase ("requires/requiring/at least/must have/you'll have/seeking
		// ...") + N(+) years [optional words] experience. Qualifiers are optional, so
		// "requires 12 years of experience" matches.
		regexp.MustCompile(`(?i)\b(?:at\s+least|minimum(?:\s+of)?|requir(?:e|es|ed|ing)|requirement:?|must\s+have|seeking|looking\s+for|ideally\s+(?:you\s+(?:have|bring))?|you(?:'ll|'ve|\s+will|\s+should|\s+must)?\s+(?:have|bring|possess|demonstrate|need))\s+(?:a\s+)?(\d{1,2})\+?\s*(?:years?|yrs?)\s+(?:of\s+)?(?:[a-z][a-z/&.-]*\s+){0,4}experience\b`),
		// Bare "N+ years [optional words] experience": the explicit + marks a minimum
		// requirement, which company-background boasts ("20 years of experience", no +)
		// do not use, so this avoids those false positives without a lead-in word.
		regexp.MustCompile(`(?i)\b(\d{1,2})\+\s*(?:years?|yrs?)\s+(?:of\s+)?(?:[a-z][a-z/&.-]*\s+){0,4}experience\b`),
		// Bare "N years <qualifier> experience" with no + and no lead-in: require an
		// explicit qualifier word (professional/industry/...) so company boasts like
		// "20 years of experience" are not treated as a requirement.
		regexp.MustCompile(`(?i)\b(\d{1,2})\s*(?:years?|yrs?)\s+(?:of\s+)?(?:professional\s+|industry\s+|relevant\s+|hands-on\s+|software\s+|engineering\s+|technical\s+|leadership\s+|work\s+|devops\s+|sre\s+|platform\s+|infrastructure\s+)+experience\b`),
		// Range "N-M years <qualifier> experience": take the lower bound as the minimum.
		regexp.MustCompile(`(?i)\b(\d{1,2})\s*(?:-|–|—|to)\s*\d{1,2}\s*(?:years?|yrs?)\s+(?:of\s+)?(?:[a-z][a-z/&.-]*\s+){0,4}experience\b`),
	}
	explicitResumeYearsRe = regexp.MustCompile(`(?i)\b(\d{1,2})\+?\s*(?:years?|yrs?)\s+(?:of\s+)?(?:professional\s+|industry\s+|relevant\s+|software\s+|engineering\s+|devops\s+|sre\s+|platform\s+|infrastructure\s+|technical\s+|work\s+)*experience\b`)
	monthRangeRe          = regexp.MustCompile(`(?i)\b(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\.?\s+(\d{4})\s*(?:-|–|—|to)\s*(present|current|now|jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t(?:ember)?)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\.?\s*(\d{4})?`)
	yearRangeRe           = regexp.MustCompile(`(?i)\b((?:19|20)\d{2})\s*(?:-|–|—|to)\s*(present|current|now|(?:19|20)\d{2})\b`)
)

var educationContextWords = []string{
	"education", "degree", "university", "college", "school", "certification", "certified", "bachelor", "master", "phd", "b.s.", "m.s.",
}

func maxRequiredExperienceYears(text string) int {
	maxYears := 0
	for _, re := range requiredYearsRes {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if len(m) < 2 {
				continue
			}
			n, err := strconv.Atoi(m[1])
			if err == nil && n > maxYears {
				maxYears = n
			}
		}
	}
	return maxYears
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
