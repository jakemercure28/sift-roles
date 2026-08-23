package discovery

// URL harvest turns apply URLs the scraper already stored into verified registry
// boards. Every listing we hold links to the employer's public ATS board, and the
// board token sits right in the URL path (job-boards.greenhouse.io/<slug>/...,
// jobs.lever.co/<slug>/..., jobs.ashbyhq.com/<slug>/..., *.myworkdayjobs.com/...).
// Extracting those boards costs zero Gemini calls and feeds the same shared
// company_registry that discovery harvests into, so a board one tenant's feed
// surfaced becomes directly scrapeable by every other tenant it matches.

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// slugShape guards against path segments that are job ids or junk rather than a
// board token. A real ATS slug starts alphanumeric and contains only lowercase
// letters, digits, and the separators ATS vendors allow.
var slugShape = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,79}$`)

// allDigits marks a purely numeric segment (a job id, never a board slug).
var allDigits = regexp.MustCompile(`^[0-9]+$`)

// srSlugShape is slugShape but case-preserving: SmartRecruiters company
// identifiers are case-SENSITIVE (jobs.smartrecruiters.com/BoschGroup), so unlike
// every other API platform the slug must not be lowercased.
var srSlugShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`)

// srSkipSegs are the highest-frequency non-company first path segments on
// jobs/careers.smartrecruiters.com (app routes and crawler files), skipped so we
// don't waste a liveness probe on them. Any other noise still gets filtered by
// the totalFound>0 existence check.
var srSkipSegs = map[string]bool{
	"oneclick-ui": true, "external-referrals": true,
	"robots.txt": true, "sitemap.xml": true,
}

// greenhouseSkipSegs are leading path segments on greenhouse.io hosts that are
// not the board token. The embed form carries the slug in the ?for= query, which
// ParseATSBoardURL checks before falling back to the first path segment.
var greenhouseSkipSegs = map[string]bool{"embed": true, "job_app": true, "v1": true, "vip": true}

// ParseATSBoardURL extracts the public ATS board embedded in a job apply URL.
// It recognizes Greenhouse, Lever, Ashby, SmartRecruiters, and Workday links and
// returns the board as a VerifiedBoard (the same shape discovery harvests). ok is
// false for URLs that are not a recognized ATS board or whose token fails the
// slug-shape guard.
func ParseATSBoardURL(rawURL string) (VerifiedBoard, bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return VerifiedBoard{}, false
	}
	host := strings.ToLower(u.Hostname())
	segs := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })

	switch {
	case strings.Contains(host, "myworkdayjobs.com"):
		if e := ParseWorkdayURL(rawURL); e != nil {
			return VerifiedBoard{Platform: "workday", Workday: e}, true
		}
	case strings.HasSuffix(host, "greenhouse.io"):
		// The embed form (boards.greenhouse.io/embed/job_app?for=<slug>) carries
		// the token in the query, not the path.
		if q := u.Query().Get("for"); q != "" {
			return apiBoard("greenhouse", q)
		}
		if slug := firstBoardSeg(segs, greenhouseSkipSegs); slug != "" {
			return apiBoard("greenhouse", slug)
		}
	case strings.HasSuffix(host, "lever.co"):
		if slug := firstBoardSeg(segs, nil); slug != "" {
			return apiBoard("lever", slug)
		}
	case strings.HasSuffix(host, "ashbyhq.com"):
		if slug := firstBoardSeg(segs, nil); slug != "" {
			return apiBoard("ashby", slug)
		}
	case host == "jobs.smartrecruiters.com" || host == "careers.smartrecruiters.com":
		// The SmartRecruiters identifier is ALWAYS the first path segment, and it
		// is case-SENSITIVE, so take segs[0] verbatim (no lowercasing) and reject
		// the URL outright when that segment is a known app route rather than a
		// company (its "slug" is never nested deeper).
		if len(segs) > 0 && !srSkipSegs[strings.ToLower(segs[0])] {
			return smartRecruitersBoard(segs[0])
		}
	}
	return VerifiedBoard{}, false
}

// firstBoardSeg returns the first path segment that looks like a board token,
// skipping vendor-specific non-token prefixes.
func firstBoardSeg(segs []string, skip map[string]bool) string {
	for _, seg := range segs {
		s := strings.ToLower(seg)
		if skip[s] {
			continue
		}
		return s
	}
	return ""
}

func apiBoard(platform, raw string) (VerifiedBoard, bool) {
	slug := strings.ToLower(strings.TrimSpace(raw))
	if !slugShape.MatchString(slug) || allDigits.MatchString(slug) {
		return VerifiedBoard{}, false
	}
	return VerifiedBoard{Platform: platform, Slug: slug}, true
}

// smartRecruitersBoard validates a case-sensitive SmartRecruiters identifier and
// returns it verbatim (no lowercasing).
func smartRecruitersBoard(raw string) (VerifiedBoard, bool) {
	slug := strings.TrimSpace(raw)
	if !srSlugShape.MatchString(slug) || allDigits.MatchString(slug) {
		return VerifiedBoard{}, false
	}
	return VerifiedBoard{Platform: "smartrecruiters", Slug: slug}, true
}

// VerifyBoards probes each board's public API concurrently and returns only the
// ones that currently exist, so a stale apply URL for a board that has since gone
// dark never enters the registry. It reuses the exact liveness checks discovery
// applies to Gemini candidates. It is best-effort: a probe error or a cancelled
// context simply drops that board rather than failing the batch.
func VerifyBoards(ctx context.Context, client *http.Client, in []VerifiedBoard, probeConcurrency int) []VerifiedBoard {
	if len(in) == 0 {
		return nil
	}
	if client == nil {
		client = &http.Client{Timeout: boardTimeout}
	}
	if probeConcurrency <= 0 {
		probeConcurrency = concurrency
	}
	live := make([]bool, len(in))
	entries := make([]*WorkdayEntry, len(in))
	_ = mapConcurrent(ctx, in, probeConcurrency, func(i int, b VerifiedBoard) error {
		if strings.ToLower(b.Platform) == "workday" {
			if b.Workday == nil {
				return nil
			}
			e, err := verifyWorkdayBoard(ctx, client, *b.Workday)
			if err == nil && e != nil {
				live[i] = true
				entries[i] = e
			}
			return nil
		}
		if ok, err := boardExists(ctx, client, strings.ToLower(b.Platform), b.Slug); err == nil {
			live[i] = ok
		}
		return nil
	})
	var out []VerifiedBoard
	for i, b := range in {
		if !live[i] {
			continue
		}
		if entries[i] != nil {
			b.Workday = entries[i]
		}
		out = append(out, b)
	}
	return out
}
