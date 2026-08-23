package searchterms

import (
	"regexp"
	"strings"
)

// maxResumeTerms bounds how many terms FromResume emits. The setup wizard shows
// them for confirmation, so the list has to stay reviewable.
const maxResumeTerms = 8

// resumeLexicon is the career-agnostic fallback vocabulary: when no line of the
// resume reads like a role heading, a plain substring sweep still recovers the
// field the person works in. It only ADDS terms the resume actually contains,
// so it never invents a target the applicant never wrote down.
var resumeLexicon = []string{
	"software engineer", "devops", "sre", "platform engineer", "data engineer", "data analyst",
	"product manager", "project manager", "program manager", "marketing", "sales", "customer success",
	"operations", "recruiter", "human resources", "accountant", "financial analyst", "nurse", "teacher",
	"merchandising", "merchandiser", "buyer", "planner", "retail", "store manager", "visual merchandising",
}

// roleLineRe matches a line that names a job role, so headings are picked up and
// prose is skipped.
var roleLineRe = regexp.MustCompile(`(?i)\b(engineer|developer|manager|director|analyst|associate|coordinator|specialist|designer|architect|consultant|administrator|operator|technician|nurse|teacher|accountant|recruiter|merchandis(?:er|ing)|buyer|planner|retail|sales|marketing|operations)\b`)

var (
	parenRe    = regexp.MustCompile(`\([^)]*\)`)
	nonTermRe  = regexp.MustCompile(`[^a-z0-9+.#/ -]+`)
	whitespace = regexp.MustCompile(`\s+`)
)

// FromResume derives target-role search terms from resume text: role-shaped
// lines first (a resume's job titles are the roles the person actually holds),
// then a lexicon sweep as the fallback. It is pure and deterministic, no LLM and
// no I/O, so it can run anywhere, including inside a background cron with no
// quota to spend.
//
// It is the shared definition behind two callers: the setup wizard, when the
// applicant leaves the search-terms box empty, and the stalled-profile repair in
// db.RepairStalledProfiles, which uses it to rescue a tenant who uploaded a
// resume but never finished the wizard. Both must derive the same terms, so a
// repaired profile is exactly what finishing the wizard would have produced.
func FromResume(resume string) []string {
	resume = strings.TrimSpace(resume)
	if resume == "" {
		return nil
	}
	seen := map[string]bool{}
	terms := []string{}
	add := func(term string) {
		term = normalizeTerm(term)
		if len(term) < 3 || seen[term] {
			return
		}
		seen[term] = true
		terms = append(terms, term)
	}
	for _, line := range strings.Split(resume, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "-* "))
		if line == "" || len(line) > 90 || strings.HasSuffix(line, ":") || strings.HasSuffix(line, ".") {
			continue
		}
		if !roleLineRe.MatchString(line) {
			continue
		}
		for _, sep := range []string{" | ", " - ", " -- ", " at ", " @ ", ","} {
			if idx := strings.Index(strings.ToLower(line), sep); idx > 0 {
				line = line[:idx]
				break
			}
		}
		add(line)
		if len(terms) >= maxResumeTerms {
			return terms
		}
	}
	lowerResume := strings.ToLower(resume)
	for _, term := range resumeLexicon {
		if strings.Contains(lowerResume, term) {
			add(term)
			if len(terms) >= maxResumeTerms {
				break
			}
		}
	}
	return terms
}

// normalizeTerm lowercases a candidate term and strips parentheticals and
// punctuation the title pre-filter would never match.
func normalizeTerm(term string) string {
	term = strings.ToLower(strings.TrimSpace(term))
	term = parenRe.ReplaceAllString(term, "")
	term = nonTermRe.ReplaceAllString(term, " ")
	term = whitespace.ReplaceAllString(term, " ")
	return strings.TrimSpace(term)
}
