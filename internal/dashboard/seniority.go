package dashboard

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// This file ports lib/seniority.js: years-of-experience parsing and seniority
// classification used by the Market Research seniority breakdown.

var yearPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(\d+)\+?\s*(?:years?|yrs?)\s*(?:of\s+)?(?:experience|exp|professional|relevant|hands-on|working|industry|related)`),
	regexp.MustCompile(`(?i)(?:experience|exp)[\s:]+(\d+)\+?\s*(?:years?|yrs?)`),
	regexp.MustCompile(`(?i)(\d+)\+?\s*(?:years?|yrs?)\s+(?:in|with|of)`),
	regexp.MustCompile(`(?i)minimum\s+(?:of\s+)?(\d+)\s*(?:years?|yrs?)`),
}

// parseYearsFromDescription returns the max plausible (1..30) YOE mentioned, or
// nil. Mirrors parseYearsFromDescription.
func parseYearsFromDescription(desc string) *int {
	if desc == "" {
		return nil
	}
	var maxYears *int
	for _, p := range yearPatterns {
		for _, m := range p.FindAllStringSubmatch(desc, -1) {
			y, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			if y > 0 && y <= 30 {
				if maxYears == nil || y > *maxYears {
					yy := y
					maxYears = &yy
				}
			}
		}
	}
	return maxYears
}

var (
	juniorTitleRe = regexp.MustCompile(`(?i)\b(junior|jr\.?|entry[- ]level|associate|intern)\b`)
	staffTitleRe  = regexp.MustCompile(`(?i)\b(staff|principal|distinguished|fellow|architect)\b`)
	leadTitleRe   = regexp.MustCompile(`(?i)\b(lead|manager|director|head|vp)\b`)
	seniorTitleRe = regexp.MustCompile(`(?i)\b(senior|sr\.?|iii)\b`)
)

func levelFromTitle(title string) string {
	if title == "" {
		return "mid"
	}
	t := strings.ToLower(title)
	switch {
	case juniorTitleRe.MatchString(t):
		return "junior"
	case staffTitleRe.MatchString(t):
		return "staff"
	case leadTitleRe.MatchString(t):
		return "staff"
	case seniorTitleRe.MatchString(t):
		return "senior"
	}
	return "mid"
}

func levelFromYears(years int) string {
	switch {
	case years <= 2:
		return "junior"
	case years <= 4:
		return "mid"
	case years <= 7:
		return "senior"
	}
	return "staff"
}

// Seniority is the classifySeniority result.
type Seniority struct {
	Level  string
	Years  *int
	Source string
}

func classifySeniority(title, description string) Seniority {
	titleLevel := levelFromTitle(title)
	years := parseYearsFromDescription(description)
	if years != nil {
		return Seniority{Level: levelFromYears(*years), Years: years, Source: "jd"}
	}
	return Seniority{Level: titleLevel, Years: nil, Source: "title"}
}

var levelRank = map[string]int{"junior": 0, "mid": 1, "senior": 2, "staff": 3}

// isAccessible reports whether the job is reachable for an applicant with the
// given YOE, mirroring isAccessible.
func isAccessible(title, description string, applicantYoe int) bool {
	return isAccessibleFor(classifySeniority(title, description), applicantYoe)
}

// isAccessibleFor is isAccessible for an already-classified job, so callers that
// have computed the Seniority once can reuse it instead of re-running the
// description regexes.
func isAccessibleFor(s Seniority, applicantYoe int) bool {
	if s.Years != nil {
		return *s.Years <= applicantYoe
	}
	return levelRank[s.Level] <= levelRank[levelFromYears(applicantYoe)]
}

var months = []string{"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"}
var monthPattern = regexp.MustCompile(`(?i)(` + strings.Join(months, "|") + `)\s+(\d{4})`)

// computeApplicantYoe reads <profileDir>/experience/*.md, finds the earliest
// "Month YYYY" mention, and returns whole years since then (relative to now), or
// nil. now is injectable for testing.
func computeApplicantYoe(profileDir string, now time.Time) *int {
	expDir := filepath.Join(profileDir, "experience")
	files, err := os.ReadDir(expDir)
	if err != nil {
		return nil
	}
	var earliest *time.Time
	monthIndex := func(name string) int {
		for i, m := range months {
			if m == name {
				return i
			}
		}
		return -1
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(expDir, f.Name()))
		if err != nil {
			continue
		}
		for _, m := range monthPattern.FindAllStringSubmatch(string(data), -1) {
			mi := monthIndex(strings.ToLower(m[1]))
			year, _ := strconv.Atoi(m[2])
			t := time.Date(year, time.Month(mi+1), 1, 0, 0, 0, 0, time.UTC)
			if earliest == nil || t.Before(*earliest) {
				tt := t
				earliest = &tt
			}
		}
	}
	if earliest == nil {
		return nil
	}
	years := int((now.Sub(*earliest).Hours()) / (365.25 * 24))
	return &years
}
