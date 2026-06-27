package dashboard

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// This file ports lib/html/job-rows.js (the job list table) to Go. Output is
// verified byte-for-byte against the Node renderer (jobrows_test.go golden).

const iconX = `<svg xmlns="http://www.w3.org/2000/svg" width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg>`

// stateAbbr is the STATE_ABBR map as an ordered list, preserving the JS insertion
// order so the abbreviation passes match the Node output exactly.
var stateAbbr = []struct{ full, abbr string }{
	{"Alabama", "AL"}, {"Alaska", "AK"}, {"Arizona", "AZ"}, {"Arkansas", "AR"},
	{"California", "CA"}, {"Colorado", "CO"}, {"Connecticut", "CT"}, {"Delaware", "DE"},
	{"Florida", "FL"}, {"Georgia", "GA"}, {"Hawaii", "HI"}, {"Idaho", "ID"},
	{"Illinois", "IL"}, {"Indiana", "IN"}, {"Iowa", "IA"}, {"Kansas", "KS"},
	{"Kentucky", "KY"}, {"Louisiana", "LA"}, {"Maine", "ME"}, {"Maryland", "MD"},
	{"Massachusetts", "MA"}, {"Michigan", "MI"}, {"Minnesota", "MN"}, {"Mississippi", "MS"},
	{"Missouri", "MO"}, {"Montana", "MT"}, {"Nebraska", "NE"}, {"Nevada", "NV"},
	{"New Hampshire", "NH"}, {"New Jersey", "NJ"}, {"New Mexico", "NM"}, {"New York", "NY"},
	{"North Carolina", "NC"}, {"North Dakota", "ND"}, {"Ohio", "OH"}, {"Oklahoma", "OK"},
	{"Oregon", "OR"}, {"Pennsylvania", "PA"}, {"Rhode Island", "RI"}, {"South Carolina", "SC"},
	{"South Dakota", "SD"}, {"Tennessee", "TN"}, {"Texas", "TX"}, {"Utah", "UT"},
	{"Vermont", "VT"}, {"Virginia", "VA"}, {"Washington", "WA"}, {"West Virginia", "WV"},
	{"Wisconsin", "WI"}, {"Wyoming", "WY"}, {"District of Columbia", "DC"},
	{"Washington DC", "DC"}, {"Washington, DC", "DC"},
}

var (
	usCountryRe   = regexp.MustCompile(`(?i)^(united states(?: of america)?|u\.?s\.?a?\.?|usa?)$`)
	remoteRe      = regexp.MustCompile(`(?i)\bremote\b|work.from.home|\bwfh\b|\banywhere\b|\bany.?location\b`)
	remoteTokenRe = regexp.MustCompile(`(?i)\bremote\b|work.from.home|\bwfh\b|\banywhere\b|\bany.?location\b`)
	numLocsRe     = regexp.MustCompile(`(?i)^\d+\s+locations?$`)
	bayAreaRe     = regexp.MustCompile(`(?i)^(san francisco |sf |s\.f\.\s*)?bay area$`)
	arrangementRe = regexp.MustCompile(`(?i)\b(only|based|located|position|role|jobs?|work|worker|employees?|candidates?|first|friendly|optional|eligible|preferred|timezones?|hours?)\b`)
	twoPlusSpace  = regexp.MustCompile(`\s{2,}`)
	leadConnRe    = regexp.MustCompile(`^[\s\-–,/|();.]+`)
	trailConnRe   = regexp.MustCompile(`[\s\-–,/|();.]+$`)
	hybridRe      = regexp.MustCompile(`(?i)\bhybrid\b`)
	inOfficeRe    = regexp.MustCompile(`(?i)\bin.?office\b`)
	usaDashRe     = regexp.MustCompile(`(?i)^USA\s*[-–,|]\s*`)
	abbrDashRe    = regexp.MustCompile(`^([A-Z]{2})\s*[-–]\s*(.+)$`)
	leadCountryRe = regexp.MustCompile(`(?i)^(United States(?: of America)?|U\.S\.A?\.?|USA?)\s*[,/]\s*`)
	trailCountry  = regexp.MustCompile(`(?i)[,]?\s*(USA?|United States(?: of America)?|U\.S\.A?\.?)\s*$`)
	parenSuffixRe = regexp.MustCompile(`(?i)\s*\([^)]{0,40}\)\s*$`)
	officeCommaRe = regexp.MustCompile(`(?i)\s+Office\s*,.*`)
	officeTailRe  = regexp.MustCompile(`(?i)\s+(Office|HQ|Headquarters|Campus|Engineering HQ)\s*$`)
	commaRunRe    = regexp.MustCompile(`(\s*,\s*)+`)
	edgeCommaRe   = regexp.MustCompile(`^[\s,]+|[\s,]+$`)
	selectLocRe   = regexp.MustCompile(`(?i)\s*:?\s*select\s+(a\s+)?location\b.*$`)
	trailEdge     = regexp.MustCompile(`[\s,]+$`)
)

func isUsCountry(s string) bool { return usCountryRe.MatchString(strings.TrimSpace(s)) }

// normalizeLocation ports the location normalizer in job-rows.js.
func normalizeLocation(loc string) string {
	if loc == "" {
		return ""
	}
	s := strings.TrimSpace(loc)

	if numLocsRe.MatchString(s) {
		return strings.ToLower(s)
	}
	if bayAreaRe.MatchString(s) {
		return "Bay Area"
	}

	remotePrefix := ""
	if remoteRe.MatchString(s) {
		rest := remoteTokenRe.ReplaceAllString(s, " ")
		rest = arrangementRe.ReplaceAllString(rest, " ")
		rest = twoPlusSpace.ReplaceAllString(rest, " ")
		rest = leadConnRe.ReplaceAllString(rest, "")
		rest = trailConnRe.ReplaceAllString(rest, "")
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return "Remote"
		}
		if isUsCountry(rest) {
			return "Remote, US"
		}
		remotePrefix = "Remote, "
		s = rest
	} else {
		if hybridRe.MatchString(s) {
			return "Hybrid"
		}
		if inOfficeRe.MatchString(s) {
			return "On-site"
		}
		if isUsCountry(s) {
			return "US"
		}
	}

	if strings.Contains(s, "|") {
		p := splitTrim(s, "|")
		if len(p) >= 2 {
			return fmt.Sprintf("%d locations", len(p))
		}
		return normalizeLocation(p[0])
	}

	if strings.Contains(s, ";") {
		p := splitTrim(s, ";")
		if len(p) >= 3 {
			return fmt.Sprintf("%d locations", len(p))
		}
		if len(p) == 2 {
			s = p[0]
		}
	}

	if strings.Contains(s, "/") {
		p := splitTrim(s, "/")
		if len(p) >= 3 {
			return fmt.Sprintf("%d locations", len(p))
		}
	}

	s = usaDashRe.ReplaceAllString(s, "")

	if m := abbrDashRe.FindStringSubmatch(s); m != nil {
		s = fmt.Sprintf("%s, %s", strings.TrimSpace(m[2]), m[1])
	}

	for _, st := range stateAbbr {
		re := regexp.MustCompile(`(?i)^` + regexp.QuoteMeta(st.full) + `\s*[-–]\s*(.+)$`)
		if m := re.FindStringSubmatch(s); m != nil {
			s = fmt.Sprintf("%s, %s", strings.TrimSpace(m[1]), st.abbr)
			break
		}
	}

	s = leadCountryRe.ReplaceAllString(s, "")
	s = trailCountry.ReplaceAllString(s, "")
	s = parenSuffixRe.ReplaceAllString(s, "")
	s = officeCommaRe.ReplaceAllString(s, "")
	s = officeTailRe.ReplaceAllString(s, "")

	for _, st := range stateAbbr {
		re := regexp.MustCompile(`(?i),\s*` + regexp.QuoteMeta(st.full) + `\b`)
		s = re.ReplaceAllString(s, ", "+st.abbr)
	}

	s = twoPlusSpace.ReplaceAllString(s, " ")
	s = commaRunRe.ReplaceAllString(s, ", ")
	s = edgeCommaRe.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)

	if remotePrefix != "" {
		if s == "" {
			return "Remote"
		}
		if isUsCountry(s) {
			s = remotePrefix + "US"
		} else {
			s = remotePrefix + s
		}
	}

	s = selectLocRe.ReplaceAllString(s, "")
	s = trailEdge.ReplaceAllString(s, "")

	if r := []rune(s); len(r) > 34 {
		s = string(r[:33]) + "…"
	}
	return s
}

func splitTrim(s, sep string) []string {
	var out []string
	for _, part := range strings.Split(s, sep) {
		if t := strings.TrimSpace(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// --- salary extraction (ports extractSalary) ---

var (
	hourlyRe = regexp.MustCompile(`(?i)\$\s*(\d{1,3}(?:\.\d{1,2})?)\s*(?:/|\s+per\s+)\s*(?:hr|hour)\b`)
	// groups: 1 USD-before-lo, 2 $-lo, 3 lo, 4 suffix-lo, 5 USD-after-lo,
	//         6 USD-before-hi, 7 $-hi, 8 hi, 9 suffix-hi, 10 annual trailer
	rangeRe = regexp.MustCompile(`(?i)(\bUSD\s*)?(\$)?\s*((?:\d{1,3}(?:,\d{3})*|\d+)(?:\.\d+)?)(?:\s*([kKmM])\b)?(\s*USD\b)?\s*(?:[-–—]+|to|up\s+to)\s*(\bUSD\s*)?(\$)?\s*((?:\d{1,3}(?:,\d{3})*|\d+)(?:\.\d+)?)(?:\s*([kKmM])\b)?\s*(USD\b|per\s*year|/\s*yr|annually|a\s*year)?`)
	// groups for max/min/single: 1 USD-before, 2 $, 3 number, 4 suffix, 5 USD-after
	maxRe    = regexp.MustCompile(`(?i)(?:up\s*to|max(?:imum)?|no\s*more\s*than)\s*(\bUSD\s*)?(\$)?\s*((?:\d{1,3}(?:,\d{3})*|\d+)(?:\.\d+)?)(?:\s*([kKmM])\b)?(\s*USD\b)?`)
	minRe    = regexp.MustCompile(`(?i)(?:from|starting\s*at|min(?:imum)?|at\s*least)\s*(\bUSD\s*)?(\$)?\s*((?:\d{1,3}(?:,\d{3})*|\d+)(?:\.\d+)?)(?:\s*([kKmM])\b)?(\s*USD\b)?`)
	singleRe = regexp.MustCompile(`(?i)(\bUSD\s*)?(\$)?\s*((?:\d{1,3}(?:,\d{3})*|\d+)(?:\.\d+)?)(?:\s*([kKmM])\b)?(\s*USD\b)?`)

	salaryCtxRe = regexp.MustCompile(`(?i)salar|compensation|\bcomp\b|remuneration|\bwage\b|base\s+pay|pay\s+(?:range|band|rate|scale|is)|pay\s*:`)
	nonUSDRe    = regexp.MustCompile(`(?i)\b(?:CAD|EUR|EU|GBP|AUD|NZD|CHF|PLN|SEK|NOK|DKK|CZK|HUF|INR|JPY|SGD|HKD|BRL|MXN|ILS|ZAR)\b|€|£|zł|₹|¥`)
)

func toK(numStr, suffix string) (int, bool) {
	v, err := strconv.ParseFloat(strings.ReplaceAll(numStr, ",", ""), 64)
	if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, false
	}
	suf := strings.ToLower(suffix)
	if suf == "m" {
		v *= 1000
	} else if suf != "k" && v >= 1000 {
		v /= 1000
	}
	return int(math.Round(v)), true
}

func okAnnual(k int, ok bool) bool { return ok && k >= 30 && k <= 600 }

func salaryLike(numStr, suffix string) bool {
	if suffix != "" || strings.Contains(numStr, ",") {
		return true
	}
	v, err := strconv.ParseFloat(numStr, 64)
	return err == nil && v >= 1000
}

// subAt returns the text of capture group i from a FindStringSubmatchIndex result.
func subAt(text string, m []int, i int) string {
	if 2*i+1 >= len(m) || m[2*i] < 0 {
		return ""
	}
	return text[m[2*i]:m[2*i+1]]
}

// salaryContextUSD reports whether the match span [start,end) sits in salary prose:
// a comp keyword shortly before it and no non-USD currency marker nearby. This lets
// bare numbers like "Base Salary starting at 145,000- 165,000" through while keeping
// counts like "serving 100,000 users" out.
func salaryContextUSD(text string, start, end int) bool {
	lo := start - 60
	if lo < 0 {
		lo = 0
	}
	// The after-window must be wide enough to catch the currency of a full
	// range when only one of its numbers matched, e.g. the "600,000" in
	// "Total compensation: 600,000 - 1,000,000 DKK".
	hi := end + 24
	if hi > len(text) {
		hi = len(text)
	}
	before, after := text[lo:start], text[end:hi]
	return salaryCtxRe.MatchString(before) && !nonUSDRe.MatchString(before) && !nonUSDRe.MatchString(after)
}

func extractSalary(desc string) string {
	if desc == "" {
		return ""
	}
	text := desc

	if m := hourlyRe.FindStringSubmatch(text); m != nil {
		if rate, err := strconv.ParseFloat(m[1], 64); err == nil && rate >= 15 && rate <= 300 {
			return "$" + trimFloat(rate) + "/hr"
		}
	}

	for _, m := range rangeRe.FindAllStringSubmatchIndex(text, -1) {
		loNum, loSuf := subAt(text, m, 3), subAt(text, m, 4)
		hiNum, hiSuf := subAt(text, m, 8), subAt(text, m, 9)
		cued := subAt(text, m, 1) != "" || subAt(text, m, 2) != "" || loSuf != "" ||
			subAt(text, m, 5) != "" || subAt(text, m, 6) != "" || subAt(text, m, 7) != "" ||
			hiSuf != "" || subAt(text, m, 10) != ""
		if !cued && !(salaryContextUSD(text, m[0], m[1]) && salaryLike(loNum, loSuf) && salaryLike(hiNum, hiSuf)) {
			continue // no currency cue and not in salary prose
		}
		lo, loOk := toK(loNum, loSuf)
		hi, hiOk := toK(hiNum, hiSuf)
		if okAnnual(lo, loOk) && okAnnual(hi, hiOk) && hi >= lo {
			return fmt.Sprintf("$%dk–$%dk", lo, hi)
		}
	}

	for _, m := range maxRe.FindAllStringSubmatchIndex(text, -1) {
		num, suf := subAt(text, m, 3), subAt(text, m, 4)
		cued := subAt(text, m, 1) != "" || subAt(text, m, 2) != "" || suf != "" || subAt(text, m, 5) != ""
		if !cued && !salaryContextUSD(text, m[0], m[1]) {
			continue
		}
		if !salaryLike(num, suf) {
			continue
		}
		if v, ok := toK(num, suf); okAnnual(v, ok) {
			return fmt.Sprintf("≤$%dk", v)
		}
	}

	for _, m := range minRe.FindAllStringSubmatchIndex(text, -1) {
		num, suf := subAt(text, m, 3), subAt(text, m, 4)
		cued := subAt(text, m, 1) != "" || subAt(text, m, 2) != "" || suf != "" || subAt(text, m, 5) != ""
		if !cued && !salaryContextUSD(text, m[0], m[1]) {
			continue
		}
		if !salaryLike(num, suf) {
			continue
		}
		if v, ok := toK(num, suf); okAnnual(v, ok) {
			return fmt.Sprintf("$%dk+", v)
		}
	}

	for _, m := range singleRe.FindAllStringSubmatchIndex(text, -1) {
		num, suf := subAt(text, m, 3), subAt(text, m, 4)
		if !salaryLike(num, suf) {
			continue
		}
		if strings.EqualFold(num, "401") && strings.EqualFold(suf, "k") {
			continue // 401k retirement plan, not a salary
		}
		// A bare k/m suffix is not cue enough on its own here ("401k", "10k requests");
		// a lone number needs an explicit currency marker or salary prose around it.
		cued := subAt(text, m, 1) != "" || subAt(text, m, 2) != ""
		if !cued && !salaryContextUSD(text, m[0], m[1]) {
			continue
		}
		if v, ok := toK(num, suf); okAnnual(v, ok) {
			return fmt.Sprintf("$%dk", v)
		}
	}

	return ""
}

// trimFloat formats an hourly rate like JS number-to-string (no trailing zeros).
func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// jsString mirrors escapeHtml(JSON.stringify(String(value))) used for inline
// onclick args. It JSON-encodes like JS (no HTML escaping of < > &) then HTML-escapes.
func jsString(value string) string {
	return escapeHTML(jsJSONString(value))
}

// jsJSONString encodes a string the way JS JSON.stringify does (quotes, backslash,
// control chars); notably it does NOT escape <, >, & (unlike Go's encoding/json).
func jsJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func detectAts(j Job) string {
	p := strings.ToLower(j.Platform)
	u := strings.ToLower(j.URL)
	switch {
	case strings.Contains(p, "greenhouse") || strings.Contains(u, "greenhouse.io"):
		return "greenhouse"
	case strings.Contains(p, "ashby") || strings.Contains(u, "ashbyhq.com"):
		return "ashby"
	case strings.Contains(p, "lever") || strings.Contains(u, "lever.co"):
		return "lever"
	case strings.Contains(p, "workday") || strings.Contains(u, "myworkdayjobs.com"):
		return "workday"
	}
	return ""
}

var monthsShort = []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}

func fmtColDate(val string) string {
	raw := formatPosted(val)
	if raw == "" || raw == "—" {
		return ""
	}
	parts := strings.Split(raw, "-")
	if len(parts) < 3 {
		return raw
	}
	mi, _ := strconv.Atoi(parts[1])
	day, _ := strconv.Atoi(parts[2])
	return fmt.Sprintf("%s %d", monthsShort[mi-1], day)
}

func renderJobTitle(j Job, eid, eTitle string) string {
	// Only emit an href for http(s) URLs. Scraped/imported leads are untrusted, and
	// escapeHTML neutralizes quoting but not the scheme, so a "javascript:" URL would
	// otherwise render a clickable XSS link. Anything else falls back to the no-URL
	// button (the saved description is still reachable).
	if u := strings.TrimSpace(j.URL); isHTTPURL(u) {
		return `<a href="` + escapeHTML(u) + `" target="_blank" rel="noreferrer">` + eTitle + `</a>`
	}
	return `<button class="job-title-missing-url" type="button" title="No posting URL stored. Show saved job description." onclick="openJobDescription(` +
		jsString(eid) + `,` + jsString(j.Title) + `,` + jsString(j.Company) + `)">` + eTitle + `</button>`
}

// isHTTPURL reports whether u is an absolute http:// or https:// URL. The scheme
// match is case-insensitive (browsers treat "JavaScript:" the same as
// "javascript:"); only these two schemes are safe to drop into an href.
func isHTTPURL(u string) bool {
	lower := strings.ToLower(u)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// parseCompanyTagsSlice normalizes/dedupes/sorts a slice of tags, matching
// parseCompanyTags(array) in lib/company-tags.js.
func parseCompanyTagsSlice(raw []string) []string {
	seen := make(map[string]struct{})
	var tags []string
	for _, r := range raw {
		t := normalizeCompanyTag(r)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		tags = append(tags, t)
	}
	sort.Strings(tags)
	return tags
}

func intPtrGt(score *int, n int) bool { return score != nil && *score > n }

// RenderJobTable ports renderJobTable. companyTags is keyed by lowercased company.
func RenderJobTable(jobs []Job, appliedByCompany map[string]int, companyTags map[string][]string, filter, sortKey string, pagination *Pagination, searchOptions SearchOptions) string {
	if filter == "" {
		filter = "all"
	}
	if sortKey == "" {
		sortKey = "score"
	}
	if len(jobs) == 0 {
		return `<div class="job-list-panel">` +
			`<div class="empty">No jobs found for this filter.</div>` +
			`</div>`
	}

	var cards strings.Builder
	for _, j := range jobs {
		cards.WriteString(renderJobCard(j, appliedByCompany, companyTags))
	}

	return `<div class="job-list-panel">` +
		`<div class="job-list filter-` + escapeHTML(filter) + `" id="job-tbody">` + cards.String() + `</div>` +
		`</div>` +
		renderJobPagination(filter, sortKey, pagination, searchOptions)
}

func renderJobCard(j Job, appliedByCompany map[string]int, companyTags map[string][]string) string {
	pval := pipelineValue(j)
	eid := escapeHTML(j.ID)
	eTitle := escapeHTML(j.Title)
	eCompany := escapeHTML(j.Company)
	jobTitleHTML := renderJobTitle(j, eid, eTitle)

	eLoc := ""
	if j.Location != "" {
		eLoc = escapeHTML(j.Location)
	}
	effectiveReasoning := j.RejectionReason
	if effectiveReasoning == "" {
		effectiveReasoning = j.Reasoning
	}
	eReasoning := ""
	if effectiveReasoning != "" {
		eReasoning = escapeHTML(effectiveReasoning)
	}
	sc := j.Score

	isRejected := j.Stage == "rejected"
	rejectedFromLabel := ""
	if j.RejectedFromStage != "" {
		if lbl, ok := pipelineLabels[j.RejectedFromStage]; ok {
			rejectedFromLabel = lbl
		} else {
			rejectedFromLabel = j.RejectedFromStage
		}
	}

	coKey := strings.TrimSpace(strings.ToLower(j.Company))

	var tagParts []string
	for _, tag := range parseCompanyTagsSlice(companyTags[coKey]) {
		cls := "complexity-badge company-tag"
		if tag == "agency" {
			cls += " tag-agency"
		}
		tagParts = append(tagParts, `<span class="`+cls+`">`+escapeHTML(tag)+`</span>`)
	}
	if isRejected && rejectedFromLabel != "" {
		tagParts = append(tagParts, `<span class="complexity-badge rejected-from">From: `+escapeHTML(rejectedFromLabel)+`</span>`)
	}
	if isRejected && j.RejectedAt != "" {
		d := j.RejectedAt
		if len(d) > 10 {
			d = d[:10]
		}
		tagParts = append(tagParts, `<span class="complexity-badge rejected-date" title="Rejected on `+escapeHTML(d)+`">`+escapeHTML(d)+`</span>`)
	}
	tagsHTML := ""
	if len(tagParts) > 0 {
		tagsHTML = `<div class="job-tags">` + strings.Join(tagParts, "") + `</div>`
	}

	salaryText := extractSalary(j.Description)

	atsName := detectAts(j)
	atsHTML := ""
	if atsName != "" {
		atsHTML = `<span class="badge-ats badge-ats-` + atsName + `">` + atsName + `</span>`
	}

	cardExtra := ""
	if intPtrGt(sc, 8) {
		cardExtra = " score-hot"
	}

	reasoningPanel := ""
	if eReasoning != "" {
		reasoningPanel = `<div class="reasoning-panel" id="reasoning-` + eid + `"><div class="reasoning-panel-inner">` + eReasoning + `</div></div>`
	}

	// Date column: applied rows show the applied date (the pill is gone), rejected
	// rows the rejection date; everything else the posting date. The tooltip keeps
	// the full history so nothing is lost by collapsing to one date.
	postedDate := fmtColDate(j.PostedAt)
	if postedDate == "" {
		postedDate = fmtColDate(j.CreatedAt)
	}
	appliedDate := fmtColDate(j.AppliedAt)
	colDate := postedDate
	dateClass := ""
	var titleParts []string
	if isRejected {
		colDate = fmtColDate(j.RejectedAt)
		if colDate == "" {
			colDate = fmtColDate(j.UpdatedAt)
		}
		if appliedDate != "" {
			titleParts = append(titleParts, "Applied "+appliedDate)
		}
		if colDate != "" {
			titleParts = append(titleParts, "Rejected "+colDate)
		}
	} else if appliedDate != "" && pval != "" {
		colDate = appliedDate
		dateClass = " job-posted-applied"
		titleParts = append(titleParts, "Applied "+appliedDate)
		if postedDate != "" {
			titleParts = append(titleParts, "Posted "+postedDate)
		}
	} else if postedDate != "" {
		titleParts = append(titleParts, "Posted "+postedDate)
	}
	if colDate == "" {
		colDate = "—"
	}
	dateTitle := ""
	if len(titleParts) > 0 {
		dateTitle = ` title="` + escapeHTML(strings.Join(titleParts, " · ")) + `"`
	}

	locText := normalizeLocation(j.Location)
	scoreCircle := renderScoreCircle(sc, eid, eReasoning)
	locTitle := ""
	if eLoc != "" {
		locTitle = ` title="` + eLoc + `"`
	}
	selectHTML := renderPipelineSelect(pval, eid)

	return fmt.Sprintf(jobCardFmt,
		cardExtra, eid, escapeHTML(j.Status),
		strconv.FormatInt(postedTimestamp(j.PostedAt), 10),
		strconv.FormatInt(postedTimestamp(j.AppliedAt), 10),
		scoreColorVar(sc),
		scoreCircle,
		jobTitleHTML,
		eCompany,
		atsHTML,
		salaryText,
		locTitle, locText,
		tagsHTML,
		dateClass, dateTitle, colDate,
		selectHTML,
		eid, iconX,
		reasoningPanel,
	)
}

func renderScoreCircle(sc *int, eid, eReasoning string) string {
	if sc == nil {
		return `<span class="score-circle ` + scoreClass(sc) + `" title="Awaiting score"><span class="score-spinner"></span></span>`
	}
	n := strconv.Itoa(*sc)
	if eReasoning != "" {
		return `<button type="button" class="score-circle ` + scoreClass(sc) + ` has-reasoning" onclick="toggleReasoning('` + eid + `')" title="View reasoning" aria-label="View reasoning for this job">` + n + `</button>`
	}
	return `<span class="score-circle ` + scoreClass(sc) + `">` + n + `</span>`
}

func selAttr(pval, val string) string {
	if pval == val {
		return "selected"
	}
	return ""
}

func renderPipelineSelect(pval, eid string) string {
	return fmt.Sprintf(pipelineSelectFmt,
		pval, eid, pipelineColor(pval),
		selAttr(pval, ""), colSlateDark,
		selAttr(pval, "applied"), colBlue,
		selAttr(pval, "phone_screen"), colInk,
		selAttr(pval, "interview"), colInk,
		selAttr(pval, "onsite"), colAmber,
		selAttr(pval, "offer"), colGreen,
		selAttr(pval, "closed"), colSlateLite,
		selAttr(pval, "rejected"), colRed,
		selAttr(pval, "ghosted"), colSlate,
	)
}

func renderJobPagination(filter, sortKey string, pagination *Pagination, searchOptions SearchOptions) string {
	if pagination == nil || pagination.TotalItems == 0 {
		return ""
	}
	page, totalPages := pagination.Page, pagination.TotalPages

	visible := map[int]struct{}{}
	for _, p := range []int{1, totalPages, page - 1, page, page + 1} {
		if p >= 1 && p <= totalPages {
			visible[p] = struct{}{}
		}
	}
	var nums []int
	for p := range visible {
		nums = append(nums, p)
	}
	sort.Ints(nums)

	var links strings.Builder
	lastPage := 0
	for _, pageNum := range nums {
		if pageNum-lastPage > 1 {
			links.WriteString(`<span class="pagination-ellipsis">...</span>`)
		}
		if pageNum == page {
			links.WriteString(`<span class="pagination-link active">` + strconv.Itoa(pageNum) + `</span>`)
		} else {
			href := buildDashboardHref(filter, sortKey, withPage(searchOptions, pageNum))
			links.WriteString(`<a class="pagination-link" href="` + href + `">` + strconv.Itoa(pageNum) + `</a>`)
		}
		lastPage = pageNum
	}

	prevLink := `<span class="pagination-btn disabled">&larr; Prev</span>`
	if page > 1 {
		prevLink = `<a class="pagination-btn" href="` + buildDashboardHref(filter, sortKey, withPage(searchOptions, page-1)) + `">&larr; Prev</a>`
	}
	nextLink := `<span class="pagination-btn disabled">Next &rarr;</span>`
	if page < totalPages {
		nextLink = `<a class="pagination-btn" href="` + buildDashboardHref(filter, sortKey, withPage(searchOptions, page+1)) + `">Next &rarr;</a>`
	}

	return fmt.Sprintf(paginationFmt, pagination.StartItem, pagination.EndItem, pagination.TotalItems, prevLink, links.String(), nextLink)
}

func withPage(o SearchOptions, page int) SearchOptions {
	o.Page = page
	return o
}

// --- exact-whitespace format strings copied from job-rows.js ---

const jobCardFmt = `
    <div class="job-card%s" data-id="%s" data-status="%s" data-posted-ts="%s" data-applied-ts="%s" style="--score-color:%s">
      <div class="job-col-score">
        %s
      </div>
      <div class="job-col-info">
        <div class="job-title">%s</div>
        <div class="job-meta">
          <span class="job-meta-co">%s</span>
          %s
          <span class="job-salary">%s</span>
          <span class="job-loc"%s>%s</span>
        </div>
        %s
      </div>
      <div class="job-col-right">
        <span class="job-posted%s"%s>%s</span>
        <div class="job-col-status">
          %s
        </div>
        <div class="job-col-actions">
          <button class="btn-row-archive" onclick="archiveJob('%s', this)" title="Archive" aria-label="Archive job">%s</button>
        </div>
      </div>
      %s
    </div>`

const pipelineSelectFmt = `<select class="pipeline-select" data-prev="%s" onchange="setPipeline('%s', this.value, this)" style="color:%s">
            <option value="" %s style="color:%s">Pending</option>
            <option value="applied" %s style="color:%s">Applied</option>
            <option value="phone_screen" %s style="color:%s">Phone Screen</option>
            <option value="interview" %s style="color:%s">Interview</option>
            <option value="onsite" %s style="color:%s">Onsite</option>
            <option value="offer" %s style="color:%s">Offer</option>
            <option value="closed" %s style="color:%s">Closed</option>
            <option value="rejected" %s style="color:%s">Rejected</option>
            <option value="ghosted" %s style="color:%s">Ghosted</option>
          </select>`

const paginationFmt = `<div class="job-pagination">
  <div class="pagination-summary">Showing %d-%d of %d</div>
  <div class="pagination-controls">
    %s
    %s
    %s
  </div>
</div>`
