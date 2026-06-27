package ats

import (
	"net/url"
	"regexp"
	"strings"
)

// primaryPlatforms mirrors PRIMARY_PLATFORMS in lib/ats-resolver.js.
var primaryPlatforms = map[string]bool{
	"ashby": true, "greenhouse": true, "lever": true, "workday": true,
}

// Job is the alternate/scraped listing being resolved, mirroring the `job`
// object passed around lib/ats-resolver.js. Only the fields the resolver reads
// are modeled.
type Job struct {
	ID          string
	Platform    string
	Title       string
	Company     string
	URL         string
	Description string
	Location    string
	PostedAt    string
}

// GreenhouseRef/AshbyRef/LeverRef/WorkdayRef are the parsed lookup components
// each ATS API needs. A nil pointer means the URL did not match.
type GreenhouseRef struct{ BoardToken, JobID string }
type AshbyRef struct{ BoardToken, JobID string }
type LeverRef struct{ Company, JobID string }
type WorkdayRef struct {
	Subdomain, Host, Board, ExternalPath string
}

// normalizePlatform maps a free-form platform label to a canonical key, ported
// from normalizePlatform in lib/ats-resolver.js.
func normalizePlatform(value string) string {
	lower := strings.ToLower(strings.TrimSpace(value))
	if lower == "" {
		return ""
	}
	switch {
	case strings.Contains(lower, "ashby"):
		return "ashby"
	case strings.Contains(lower, "greenhouse"):
		return "greenhouse"
	case strings.Contains(lower, "lever"):
		return "lever"
	case strings.Contains(lower, "workday"), strings.Contains(lower, "myworkdayjobs"):
		return "workday"
	}
	return lower
}

// displayPlatform returns the human-facing platform name, ported from
// displayPlatform in lib/ats-resolver.js.
func displayPlatform(value string) string {
	switch normalizePlatform(value) {
	case "ashby":
		return "Ashby"
	case "greenhouse":
		return "Greenhouse"
	case "lever":
		return "Lever"
	case "workday":
		return "Workday"
	}
	return value
}

// isPrimaryPlatform reports whether value is one of the four primary ATSes.
func isPrimaryPlatform(value string) bool {
	return primaryPlatforms[normalizePlatform(value)]
}

// ---------------------------------------------------------------------------
// ATS detection (ported from lib/atsDetector.js)
// ---------------------------------------------------------------------------

type atsPattern struct {
	platform string
	re       *regexp.Regexp
}

var atsPatterns = []atsPattern{
	{"Ashby", regexp.MustCompile(`^https?://jobs\.ashbyhq\.com/([^/?#]+)`)},
	{"Greenhouse", regexp.MustCompile(`^https?://(?:boards|job-boards)\.greenhouse\.io/([^/?#]+)`)},
	{"Lever", regexp.MustCompile(`^https?://jobs\.lever\.co/([^/?#]+)`)},
	{"Workable", regexp.MustCompile(`^https?://apply\.workable\.com/([^/?#]+)`)},
	{"Workday", regexp.MustCompile(`^https?://([^/.]+)\.(?:wd\d+\.|)myworkdayjobs\.com/`)},
	{"Rippling", regexp.MustCompile(`^https?://ats\.rippling\.com/([^/?#]+)/`)},
}

var reGhJid = regexp.MustCompile(`(?i)[?&]gh_jid=\d+`)

// AtsMatch is the result of detectAts: the detected platform and (when present)
// the matched company/board token.
type AtsMatch struct {
	Platform string
	Company  string
}

// detectAts detects a direct ATS link, ported from detectAts in lib/atsDetector.js.
func detectAts(u string) *AtsMatch {
	if u == "" {
		return nil
	}
	if reGhJid.MatchString(u) {
		return &AtsMatch{Platform: "Greenhouse"}
	}
	for _, p := range atsPatterns {
		if m := p.re.FindStringSubmatch(u); m != nil {
			return &AtsMatch{Platform: p.platform, Company: m[1]}
		}
	}
	return nil
}

// detectJobPlatform infers a job's platform from its URL, id prefix, or platform
// label, ported from detectJobPlatform in lib/ats-resolver.js. Empty string
// means undetermined.
func detectJobPlatform(job Job) string {
	if ats := detectAts(job.URL); ats != nil && ats.Platform != "" {
		return strings.ToLower(ats.Platform)
	}
	lowerID := strings.ToLower(job.ID)
	switch {
	case strings.HasPrefix(lowerID, "greenhouse-"):
		return "greenhouse"
	case strings.HasPrefix(lowerID, "lever-"):
		return "lever"
	case strings.HasPrefix(lowerID, "ashby-"):
		return "ashby"
	case strings.HasPrefix(lowerID, "workday-"):
		return "workday"
	case strings.HasPrefix(lowerID, "rippling-"):
		return "rippling"
	}
	return normalizePlatform(job.Platform)
}

var reEmail = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// classifyUnsupportedURL labels a known non-primary ATS / aggregator URL, ported
// from classifyUnsupportedUrl in lib/ats-resolver.js. Empty string means the URL
// is not a recognized unsupported host.
func classifyUnsupportedURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	host := strings.ToLower(rawURL)
	if parsed, err := url.Parse(rawURL); err == nil && parsed.Hostname() != "" {
		host = strings.ToLower(parsed.Hostname())
	}
	full := strings.ToLower(rawURL)

	switch {
	case strings.Contains(host, "ats.rippling.com"):
		return "Rippling"
	case strings.Contains(host, "apply.workable.com"):
		return "Workable"
	case strings.Contains(host, "icims.com"):
		return "iCIMS"
	case strings.Contains(host, "oraclecloud.com"):
		return "Oracle Cloud"
	case strings.Contains(host, "ultipro.com"), strings.Contains(host, "ukg.com"):
		return "UKG"
	case strings.Contains(host, "bamboohr.com"):
		return "BambooHR"
	case strings.Contains(host, "applytojob.com"):
		return "JazzHR"
	case strings.Contains(host, "linkedin.com"):
		return "LinkedIn"
	case strings.Contains(host, "servicenow.com"):
		return "ServiceNow Careers"
	case host == "search-careers.gm.com", host == "careers.draftkings.com":
		return "Company Careers"
	case strings.Contains(host, "remoteok.com"):
		return "RemoteOK"
	case strings.Contains(host, "builtin.com"), strings.Contains(host, "builtinseattle.com"):
		return "Built In"
	case reEmail.MatchString(full):
		return "Email"
	}
	return ""
}

// ---------------------------------------------------------------------------
// URL parsers
// ---------------------------------------------------------------------------

var (
	reGreenhouseStandard = regexp.MustCompile(`(?i)greenhouse\.io/([^/?#]+)/jobs/(\d+)`)
	reGreenhouseJid      = regexp.MustCompile(`(?i)[?&]gh_jid=(\d+)`)
	reAshby              = regexp.MustCompile(`(?i)jobs\.ashbyhq\.com/([^/?#]+)/([0-9a-f-]{36})`)
	reLever              = regexp.MustCompile(`(?i)jobs\.lever\.co/([^/?#]+)/([0-9a-f-]{36})`)
	reWorkdayHost        = regexp.MustCompile(`(?i)^([^/.]+)\.(?:wd\d+\.)?myworkdayjobs\.com$`)
)

// parseGreenhouseURL parses a Greenhouse board/job URL, ported from
// parseGreenhouseUrl in lib/greenhouse-url.js (using normalizeSlugPart as the
// slugifier, matching parseGreenhouseUrl in lib/ats-resolver.js). A gh_jid query
// param falls back to slugifying fallbackCompany for the board token.
func parseGreenhouseURL(rawURL, fallbackCompany string) *GreenhouseRef {
	if rawURL == "" {
		return nil
	}
	if m := reGreenhouseStandard.FindStringSubmatch(rawURL); m != nil {
		return &GreenhouseRef{BoardToken: m[1], JobID: m[2]}
	}
	if m := reGreenhouseJid.FindStringSubmatch(rawURL); m != nil {
		boardToken := normalizeSlugPart(fallbackCompany)
		if boardToken == "" {
			return nil
		}
		return &GreenhouseRef{BoardToken: boardToken, JobID: m[1]}
	}
	return nil
}

// parseGreenhouseJob resolves a Greenhouse ref from a job's URL or its
// "greenhouse-<id>" id, ported from parseGreenhouseJob in lib/greenhouse-url.js.
func parseGreenhouseJob(job Job) *GreenhouseRef {
	if ref := parseGreenhouseURL(job.URL, job.Company); ref != nil {
		return ref
	}
	if strings.HasPrefix(job.ID, "greenhouse-") {
		boardToken := normalizeSlugPart(job.Company)
		if boardToken == "" {
			return nil
		}
		return &GreenhouseRef{BoardToken: boardToken, JobID: strings.TrimPrefix(job.ID, "greenhouse-")}
	}
	return nil
}

// parseAshbyURL parses an Ashby job URL, ported from parseAshbyUrl.
func parseAshbyURL(rawURL string) *AshbyRef {
	if m := reAshby.FindStringSubmatch(rawURL); m != nil {
		return &AshbyRef{BoardToken: m[1], JobID: m[2]}
	}
	return nil
}

// parseLeverURL parses a Lever job URL, ported from parseLeverUrl.
func parseLeverURL(rawURL string) *LeverRef {
	if m := reLever.FindStringSubmatch(rawURL); m != nil {
		return &LeverRef{Company: m[1], JobID: m[2]}
	}
	return nil
}

// parseWorkdayURL parses a Workday job URL into CXS lookup components, ported
// from parseWorkdayUrl in lib/ats-resolver.js.
func parseWorkdayURL(rawURL string) *WorkdayRef {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	host := parsed.Hostname()
	hostMatch := reWorkdayHost.FindStringSubmatch(host)
	if hostMatch == nil {
		return nil
	}
	var parts []string
	for _, p := range strings.Split(parsed.Path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	boardIndex := -1
	for i, p := range parts {
		if p != "en-US" && p != "en" && p != "jobs" {
			boardIndex = i
			break
		}
	}
	if boardIndex < 0 || parts[boardIndex] == "" {
		return nil
	}
	board := parts[boardIndex]
	externalPath := "/" + strings.Join(parts[boardIndex+1:], "/")
	if externalPath == "" || externalPath == "/" {
		return nil
	}
	return &WorkdayRef{Subdomain: hostMatch[1], Host: host, Board: board, ExternalPath: externalPath}
}

// ---------------------------------------------------------------------------
// Slug / title helpers
// ---------------------------------------------------------------------------

var (
	reAmp           = regexp.MustCompile(`&`)
	reParen         = regexp.MustCompile(`\([^)]*\)`)
	reSlugStopWords = regexp.MustCompile(`\b(inc|incorporated|llc|ltd|co|corp|corporation|company|ai|technologies|technology)\b`)
	reNonAlnum      = regexp.MustCompile(`[^a-z0-9]+`)
)

// normalizeSlugPart ports normalizeSlugPart in lib/ats-resolver.js.
func normalizeSlugPart(value string) string {
	s := strings.ToLower(value)
	s = reAmp.ReplaceAllString(s, "and")
	s = reParen.ReplaceAllString(s, "")
	s = reSlugStopWords.ReplaceAllString(s, "")
	s = reNonAlnum.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

var slugWordStops = map[string]bool{
	"inc": true, "incorporated": true, "llc": true, "ltd": true,
	"co": true, "corp": true, "corporation": true, "company": true,
}

// slugCandidates ports slugCandidates in lib/ats-resolver.js: the normalized
// slug plus hyphenated/concatenated word forms, de-duplicated, blanks removed.
func slugCandidates(company string) []string {
	normalized := normalizeSlugPart(company)
	lower := strings.ToLower(company)
	lower = strings.ReplaceAll(lower, "&", " and ")
	lower = reParen.ReplaceAllString(lower, "")
	var words []string
	for _, w := range reNonAlnum.Split(lower, -1) {
		if w != "" && !slugWordStops[w] {
			words = append(words, w)
		}
	}
	candidates := uniqueNonEmpty([]string{
		normalized,
		strings.Join(words, "-"),
		strings.Join(words, ""),
	})
	if strings.Contains(strings.ToLower(company), "ujet") {
		candidates = appendUnique(candidates, "ujet")
	}
	return candidates
}

// greenhouseBoardCandidates ports greenHouseBoardCandidates in lib/ats-resolver.js.
func greenhouseBoardCandidates(company string) []string {
	slug := normalizeSlugPart(company)
	candidates := uniqueNonEmpty([]string{slug})
	if strings.Contains(strings.ToLower(company), "ujet") {
		candidates = appendUnique(candidates, "ujet")
	}
	return candidates
}

var titleStopTokens = map[string]bool{
	"senior": true, "staff": true, "lead": true, "engineer": true, "remote": true,
}

// titleTokens ports titleTokens in lib/ats-resolver.js.
func titleTokens(title string) []string {
	lower := strings.ToLower(title)
	lower = reNonAlnum.ReplaceAllString(lower, " ")
	var out []string
	for _, tok := range strings.Fields(lower) {
		if len(tok) >= 3 && !titleStopTokens[tok] {
			out = append(out, tok)
		}
	}
	return out
}

// titleMatches ports titleMatches in lib/ats-resolver.js: exact match, or enough
// distinctive tokens of `expected` appear in `candidate`.
func titleMatches(candidate, expected string) bool {
	left := strings.ToLower(candidate)
	right := strings.ToLower(expected)
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	tokens := titleTokens(expected)
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

// uniqueNonEmpty returns the input order-preserved, de-duplicated, blanks removed.
func uniqueNonEmpty(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func appendUnique(values []string, v string) []string {
	for _, existing := range values {
		if existing == v {
			return values
		}
	}
	return append(values, v)
}
