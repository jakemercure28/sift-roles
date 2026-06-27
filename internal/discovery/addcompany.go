package discovery

// Add ONE company to suggested-companies.json after verifying it has a live ATS
// board, so the scraper picks it up on the next run. Ported from
// scripts/add-company.js (the /add-company command): it resolves
// Greenhouse/Ashby/Lever by slug and Workday by its myworkdayjobs.com URL,
// reusing the same board-verification helpers company discovery uses. Nothing is
// written unless the board actually verifies.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AddCompanyArgs are the resolved /add-company inputs.
type AddCompanyArgs struct {
	Name     string
	URL      string
	Slug     string
	Platform string
}

// AddCompanyResult reports what (if anything) was added. Code mirrors the JS exit
// codes: 0 added or already present, 1 board did not verify, 2 bad usage.
type AddCompanyResult struct {
	OK       bool
	Code     int
	Reason   string
	Added    bool
	Already  bool
	Platform string
	ID       string
	Name     string
}

type atsSlug struct {
	Platform string
	Slug     string
}

// slugFromATSURL pulls {platform, slug} out of a Greenhouse/Ashby/Lever board
// URL. Returns nil for anything else (including Workday, handled separately via
// ParseWorkdayURL). Ported from slugFromAtsUrl in add-company.js.
func slugFromATSURL(raw string) *atsSlug {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	segs := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	first := ""
	if len(segs) > 0 {
		first = segs[0]
	}

	switch {
	// boards.greenhouse.io/<slug>, job-boards.greenhouse.io/<slug>
	case strings.HasSuffix(host, "greenhouse.io") && first != "" &&
		(host == "boards.greenhouse.io" || host == "job-boards.greenhouse.io"):
		return &atsSlug{Platform: "greenhouse", Slug: first}
	// jobs.lever.co/<slug>, api.lever.co/v0/postings/<slug>
	case strings.HasSuffix(host, "lever.co") && len(segs) > 0:
		slug := first
		for i, s := range segs {
			if s == "postings" && i+1 < len(segs) {
				slug = segs[i+1]
				break
			}
		}
		if slug == "" {
			return nil
		}
		return &atsSlug{Platform: "lever", Slug: slug}
	// jobs.ashbyhq.com/<slug>, <slug>.ashbyhq.com
	case strings.HasSuffix(host, "ashbyhq.com"):
		if first != "" {
			return &atsSlug{Platform: "ashby", Slug: first}
		}
		sub := strings.TrimSuffix(strings.TrimSuffix(host, "ashbyhq.com"), ".")
		if sub != "" && sub != "jobs" {
			return &atsSlug{Platform: "ashby", Slug: sub}
		}
		return nil
	// <slug>.greenhouse.io subdomain boards
	case strings.HasSuffix(host, "greenhouse.io"):
		sub := strings.TrimSuffix(strings.TrimSuffix(host, "greenhouse.io"), ".")
		if sub != "" && sub != "boards" && sub != "job-boards" {
			return &atsSlug{Platform: "greenhouse", Slug: sub}
		}
		return nil
	}
	return nil
}

func apiList(s *Suggested, platform string) *[]string {
	switch platform {
	case "greenhouse":
		return &s.Greenhouse
	case "ashby":
		return &s.Ashby
	case "lever":
		return &s.Lever
	}
	return nil
}

// addAPIBoard appends an API-board slug to its list if not already present
// (slug-variant aware via SlugKey). Returns (added, already).
func addAPIBoard(s *Suggested, platform, slug string) (bool, bool) {
	list := apiList(s, platform)
	if list == nil {
		return false, false
	}
	key := SlugKey(slug)
	for _, existing := range *list {
		if SlugKey(existing) == key {
			return false, true
		}
	}
	*list = append(*list, slug)
	return true, false
}

// addWorkdayBoardEntry appends a verified Workday entry, deduped by tenant+board
// (WorkdayKey) so brand boards on one tenant coexist. Returns (added, already).
func addWorkdayBoardEntry(s *Suggested, entry WorkdayEntry) (bool, bool) {
	key := WorkdayKey(entry)
	for _, e := range s.Workday {
		if WorkdayKey(e) == key {
			return false, true
		}
	}
	s.Workday = append(s.Workday, entry)
	return true, false
}

// AddCompany verifies and (if live) records one company's ATS board under
// dataDir/suggested-companies.json. client is optional; a 10s client is used
// when nil.
func AddCompany(ctx context.Context, dataDir string, client *http.Client, args AddCompanyArgs) (AddCompanyResult, error) {
	if client == nil {
		client = &http.Client{Timeout: boardTimeout}
	}
	name := strings.TrimSpace(args.Name)
	rawURL := strings.TrimSpace(args.URL)
	explicitSlug := strings.TrimSpace(args.Slug)
	explicitPlatform := strings.ToLower(strings.TrimSpace(args.Platform))

	suggested := LoadSuggested(dataDir)

	// 1) Workday: needs the real myworkdayjobs.com URL to derive sub/wd/board.
	if rawURL != "" && strings.Contains(strings.ToLower(rawURL), ".myworkdayjobs.com") {
		parsed := ParseWorkdayURL(rawURL)
		if parsed == nil {
			return AddCompanyResult{Code: 1, Reason: "Could not parse a Workday board from the URL. Expected https://<tenant>.wdN.myworkdayjobs.com/<Board>."}, nil
		}
		label := name
		if label == "" {
			label = parsed.Sub
		}
		parsed.Label = label
		entry, err := verifyWorkdayBoard(ctx, client, *parsed)
		if err != nil {
			return AddCompanyResult{}, err
		}
		if entry == nil {
			return AddCompanyResult{Code: 1, Reason: "Workday board did not verify (no live jobs API) for " + rawURL + ". Double-check the tenant, wdN, and board path."}, nil
		}
		added, already := addWorkdayBoardEntry(&suggested, *entry)
		if err := persist(dataDir, &suggested, added); err != nil {
			return AddCompanyResult{}, err
		}
		return AddCompanyResult{OK: true, Code: 0, Added: added, Already: already, Platform: "workday", ID: entry.Sub + "/" + entry.Board, Name: entry.Label}, nil
	}

	// 2) Greenhouse/Ashby/Lever: resolve by slug (from --slug, a board URL, or name).
	platform := ""
	if isAPIPlatform(explicitPlatform) {
		platform = explicitPlatform
	}
	rawSlug := explicitSlug
	if rawSlug == "" && rawURL != "" {
		if fromURL := slugFromATSURL(rawURL); fromURL != nil {
			if platform == "" {
				platform = fromURL.Platform
			}
			rawSlug = fromURL.Slug
		}
	}
	if rawSlug == "" {
		rawSlug = name
	}
	if rawSlug == "" {
		return AddCompanyResult{Code: 2, Reason: "Nothing to resolve. Pass a company --name, a --slug, or a board --url."}, nil
	}

	resolved, err := resolveAPIBoard(ctx, client, platform, rawSlug)
	if err != nil {
		return AddCompanyResult{}, err
	}
	if !resolved.Exists {
		return AddCompanyResult{Code: 1, Reason: `No Greenhouse/Ashby/Lever board found for "` + rawSlug + `". If this company is on Workday, pass its --url (https://<tenant>.wdN.myworkdayjobs.com/<Board>).`}, nil
	}
	added, already := addAPIBoard(&suggested, resolved.Platform, resolved.Slug)
	if err := persist(dataDir, &suggested, added); err != nil {
		return AddCompanyResult{}, err
	}
	displayName := name
	if displayName == "" {
		displayName = resolved.Slug
	}
	return AddCompanyResult{OK: true, Code: 0, Added: added, Already: already, Platform: resolved.Platform, ID: resolved.Slug, Name: displayName}, nil
}

func persist(dataDir string, s *Suggested, added bool) error {
	if !added {
		return nil
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	s.UpdatedAt = &now
	return SaveSuggested(dataDir, *s)
}
