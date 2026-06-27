package ats

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// fetchTimeout mirrors FETCH_TIMEOUT_MS in config/constants.js.
const fetchTimeout = 12 * time.Second

// defaultHeaders mirrors DEFAULT_HEADERS in lib/ats-resolver.js.
var defaultHeaders = map[string]string{
	"User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Safari/537.36",
	"Accept":     "text/html,application/xhtml+xml,application/json",
}

// fetchResult is a successful (HTTP 2xx) fetch: the response body and the final
// URL after redirects.
type fetchResult struct {
	Body     []byte
	FinalURL string
}

// Fetcher performs an HTTP GET, returning nil (no error) for any non-2xx
// response, mirroring safeFetch in lib/utils.js (which swallows blocking/error
// responses and returns null). It is injected so resolution runs offline in tests.
// A non-nil error is reserved for context cancellation / unrecoverable transport
// failures that should abort the whole resolution.
type Fetcher func(ctx context.Context, url, label string) (*fetchResult, error)

// httpFetcher is the production Fetcher: a GET with the default headers and a
// per-request timeout, following redirects (http.Client default). Non-2xx
// responses and transport errors resolve to (nil, nil) like safeFetch.
func httpFetcher(client *http.Client) Fetcher {
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	return func(ctx context.Context, url, label string) (*fetchResult, error) {
		ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, nil
		}
		for k, v := range defaultHeaders {
			req.Header.Set(k, v)
		}
		res, err := client.Do(req)
		if err != nil {
			if ctx.Err() == context.Canceled {
				return nil, ctx.Err()
			}
			return nil, nil // swallow transport errors, matching safeFetch
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return nil, nil
		}
		body, err := io.ReadAll(res.Body)
		if err != nil {
			return nil, nil
		}
		finalURL := url
		if res.Request != nil && res.Request.URL != nil {
			finalURL = res.Request.URL.String()
		}
		return &fetchResult{Body: body, FinalURL: finalURL}, nil
	}
}

// ---------------------------------------------------------------------------
// Canonical job + normalize
// ---------------------------------------------------------------------------

// CanonicalJob is a normalized primary ATS posting, mirroring the object built
// by normalizeCanonicalJob in lib/ats-resolver.js (resolution.job).
type CanonicalJob struct {
	ID          string
	Platform    string // display platform ("Greenhouse", etc.)
	Title       string
	Company     string
	URL         string
	PostedAt    string
	Description string
	Location    string
}

// normalizeCanonicalJob fills blank fields from the original job and sets the
// display platform, ported from normalizeCanonicalJob in lib/ats-resolver.js.
func normalizeCanonicalJob(c CanonicalJob, fallback Job) *CanonicalJob {
	return &CanonicalJob{
		ID:          c.ID,
		Platform:    displayPlatform(c.Platform),
		Title:       firstNonEmpty(c.Title, fallback.Title),
		Company:     firstNonEmpty(c.Company, fallback.Company),
		URL:         firstNonEmpty(c.URL, fallback.URL),
		PostedAt:    firstNonEmpty(c.PostedAt, fallback.PostedAt),
		Description: firstNonEmpty(c.Description, fallback.Description),
		Location:    firstNonEmpty(c.Location, fallback.Location),
	}
}

// ---------------------------------------------------------------------------
// ATS API response shapes
// ---------------------------------------------------------------------------

type ghLocation struct {
	Name string `json:"name"`
}
type ghJob struct {
	ID          json.Number `json:"id"`
	Title       string      `json:"title"`
	AbsoluteURL string      `json:"absolute_url"`
	UpdatedAt   string      `json:"updated_at"`
	Content     string      `json:"content"`
	Location    ghLocation  `json:"location"`
}
type ghBoard struct {
	Jobs []ghJob `json:"jobs"`
}

type ashbyJob struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	JobURL           string `json:"jobUrl"`
	PublishedDate    string `json:"publishedDate"`
	PublishedAt      string `json:"publishedAt"`
	UpdatedAt        string `json:"updatedAt"`
	DescriptionHTML  string `json:"descriptionHtml"`
	DescriptionPlain string `json:"descriptionPlain"`
	Description      string `json:"description"`
	Location         string `json:"location"`
	LocationName     string `json:"locationName"`
	CompanyName      string `json:"companyName"`
}
type ashbyBoard struct {
	Jobs []ashbyJob `json:"jobs"`
}

type leverList struct {
	Text    string `json:"text"`
	Content string `json:"content"`
}
type leverCategories struct {
	Location string `json:"location"`
}
type leverJob struct {
	ID               string          `json:"id"`
	Text             string          `json:"text"`
	HostedURL        string          `json:"hostedUrl"`
	CreatedAt        int64           `json:"createdAt"`
	DescriptionPlain string          `json:"descriptionPlain"`
	Lists            []leverList     `json:"lists"`
	Categories       leverCategories `json:"categories"`
}

type workdayInfo struct {
	Title          string `json:"title"`
	JobDescription string `json:"jobDescription"`
	StartDate      string `json:"startDate"`
	PostedOn       string `json:"postedOn"`
	Location       string `json:"location"`
	LocationsText  string `json:"locationsText"`
}
type workdayResp struct {
	JobPostingInfo *workdayInfo `json:"jobPostingInfo"`
	workdayInfo                 // flat fields for the `|| data` fallback
}

// ---------------------------------------------------------------------------
// Single-URL fetchers
// ---------------------------------------------------------------------------

// fetchGreenhouseJob resolves a Greenhouse job by its board/job URL via the
// boards-api, ported from fetchGreenhouseJob in lib/ats-resolver.js.
func fetchGreenhouseJob(ctx context.Context, fetch Fetcher, rawURL string, fallback Job) (*CanonicalJob, error) {
	parsed := parseGreenhouseURL(rawURL, fallback.Company)
	if parsed == nil {
		return nil, nil
	}
	apiURL := fmt.Sprintf("https://boards-api.greenhouse.io/v1/boards/%s/jobs/%s?content=true", parsed.BoardToken, parsed.JobID)
	res, err := fetch(ctx, apiURL, "greenhouse-resolve/"+parsed.BoardToken+"/"+parsed.JobID)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	var data ghJob
	if jsonErr := json.Unmarshal(res.Body, &data); jsonErr != nil {
		return nil, nil
	}
	if data.ID.String() == "" && data.Title == "" {
		return nil, nil
	}
	url := firstNonEmpty(data.AbsoluteURL, rawURL)
	return normalizeCanonicalJob(CanonicalJob{
		ID:          "greenhouse-" + parsed.JobID,
		Platform:    "Greenhouse",
		Title:       data.Title,
		Company:     parsed.BoardToken,
		URL:         url,
		PostedAt:    data.UpdatedAt,
		Description: stripHTML(data.Content),
		Location:    data.Location.Name,
	}, fallback), nil
}

// fetchAshbyJob resolves an Ashby job by its board/job URL, ported from
// fetchAshbyJob in lib/ats-resolver.js.
func fetchAshbyJob(ctx context.Context, fetch Fetcher, rawURL string, fallback Job) (*CanonicalJob, error) {
	parsed := parseAshbyURL(rawURL)
	if parsed == nil {
		return nil, nil
	}
	apiURL := fmt.Sprintf("https://api.ashbyhq.com/posting-api/job-board/%s?includeCompensation=true", parsed.BoardToken)
	res, err := fetch(ctx, apiURL, "ashby-resolve/"+parsed.BoardToken)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	var board ashbyBoard
	if jsonErr := json.Unmarshal(res.Body, &board); jsonErr != nil {
		return nil, nil
	}
	var match *ashbyJob
	for i := range board.Jobs {
		if board.Jobs[i].ID == parsed.JobID {
			match = &board.Jobs[i]
			break
		}
	}
	if match == nil {
		return nil, nil
	}
	return normalizeCanonicalJob(CanonicalJob{
		ID:          "ashby-" + parsed.JobID,
		Platform:    "Ashby",
		Title:       match.Title,
		Company:     parsed.BoardToken,
		URL:         firstNonEmpty(match.JobURL, rawURL),
		PostedAt:    firstNonEmpty(match.PublishedDate, match.UpdatedAt),
		Description: stripHTML(firstNonEmpty(match.DescriptionHTML, match.DescriptionPlain, match.Description)),
		Location:    firstNonEmpty(match.Location, match.LocationName),
	}, fallback), nil
}

// fetchLeverJob resolves a Lever job by its URL, ported from fetchLeverJob.
func fetchLeverJob(ctx context.Context, fetch Fetcher, rawURL string, fallback Job) (*CanonicalJob, error) {
	parsed := parseLeverURL(rawURL)
	if parsed == nil {
		return nil, nil
	}
	apiURL := fmt.Sprintf("https://api.lever.co/v0/postings/%s/%s", parsed.Company, parsed.JobID)
	res, err := fetch(ctx, apiURL, "lever-resolve/"+parsed.Company+"/"+parsed.JobID)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	var data leverJob
	if jsonErr := json.Unmarshal(res.Body, &data); jsonErr != nil {
		return nil, nil
	}
	if data.ID == "" && data.Text == "" {
		return nil, nil
	}
	return normalizeCanonicalJob(CanonicalJob{
		ID:          "lever-" + parsed.JobID,
		Platform:    "Lever",
		Title:       data.Text,
		Company:     parsed.Company,
		URL:         firstNonEmpty(data.HostedURL, rawURL),
		PostedAt:    leverISOTime(data.CreatedAt),
		Description: stripHTML(leverDescription(data.DescriptionPlain, data.Lists)),
		Location:    data.Categories.Location,
	}, fallback), nil
}

// fetchWorkdayJob resolves a Workday job via the CXS detail endpoint, ported
// from fetchWorkdayJob in lib/ats-resolver.js.
func fetchWorkdayJob(ctx context.Context, fetch Fetcher, rawURL string, fallback Job) (*CanonicalJob, error) {
	parsed := parseWorkdayURL(rawURL)
	if parsed == nil {
		return nil, nil
	}
	detailURL := fmt.Sprintf("https://%s/wday/cxs/%s/%s%s", parsed.Host, parsed.Subdomain, parsed.Board, parsed.ExternalPath)
	res, err := fetch(ctx, detailURL, "workday-resolve/"+parsed.Subdomain+"/"+parsed.Board)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	var resp workdayResp
	if jsonErr := json.Unmarshal(res.Body, &resp); jsonErr != nil {
		return nil, nil
	}
	info := resp.JobPostingInfo
	if info == nil {
		info = &resp.workdayInfo
	}
	if info.Title == "" && info.JobDescription == "" {
		return nil, nil
	}
	// Mirror `externalPath.split('_').pop() || externalPath.split('/').pop()`:
	// the segment after the last '_', falling back to after the last '/' only
	// when the former is empty (the path ends in '_').
	workdayID := lastAfter(parsed.ExternalPath, "_")
	if workdayID == "" {
		workdayID = lastAfter(parsed.ExternalPath, "/")
	}
	return normalizeCanonicalJob(CanonicalJob{
		ID:          fmt.Sprintf("workday-%s-%s", parsed.Subdomain, workdayID),
		Platform:    "Workday",
		Title:       info.Title,
		Company:     firstNonEmpty(fallback.Company, parsed.Subdomain),
		URL:         rawURL,
		PostedAt:    firstNonEmpty(info.StartDate, info.PostedOn),
		Description: stripHTML(info.JobDescription),
		Location:    firstNonEmpty(info.Location, info.LocationsText),
	}, fallback), nil
}

// fetchPrimaryJob dispatches to the right ATS fetcher by platform, ported from
// fetchPrimaryJob in lib/ats-resolver.js.
func fetchPrimaryJob(ctx context.Context, fetch Fetcher, platform, rawURL string, fallback Job) (*CanonicalJob, error) {
	switch normalizePlatform(platform) {
	case "greenhouse":
		return fetchGreenhouseJob(ctx, fetch, rawURL, fallback)
	case "ashby":
		return fetchAshbyJob(ctx, fetch, rawURL, fallback)
	case "lever":
		return fetchLeverJob(ctx, fetch, rawURL, fallback)
	case "workday":
		return fetchWorkdayJob(ctx, fetch, rawURL, fallback)
	}
	return nil, nil
}

// primaryRef is the platform info detected from a primary ATS URL.
type primaryRef struct {
	platform        string
	displayPlatform string
	company         string
}

// primaryFromURL detects whether a URL is a primary ATS link, ported from
// primaryFromUrl in lib/ats-resolver.js.
func primaryFromURL(rawURL string) *primaryRef {
	ats := detectAts(rawURL)
	if ats == nil || !isPrimaryPlatform(ats.Platform) {
		return nil
	}
	return &primaryRef{
		platform:        normalizePlatform(ats.Platform),
		displayPlatform: displayPlatform(ats.Platform),
		company:         ats.Company,
	}
}

// textResult is the body+final-URL returned by fetchText.
type textResult struct {
	Text     string
	FinalURL string
}

// fetchText GETs a page (with default headers, following redirects) and returns
// its body and final URL, ported from fetchText in lib/ats-resolver.js.
func fetchText(ctx context.Context, fetch Fetcher, rawURL, label string) (*textResult, error) {
	if label == "" {
		label = "ats-resolver"
	}
	res, err := fetch(ctx, rawURL, label)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	finalURL := res.FinalURL
	if finalURL == "" {
		finalURL = rawURL
	}
	return &textResult{Text: string(res.Body), FinalURL: finalURL}, nil
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// leverISOTime mirrors `data.createdAt ? new Date(data.createdAt).toISOString() : ”`.
func leverISOTime(createdAtMs int64) string {
	if createdAtMs == 0 {
		return ""
	}
	return time.UnixMilli(createdAtMs).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// leverDescription joins the plain description with each list's text+content,
// mirroring the array build in fetchLeverJob/searchLeverBoard.
func leverDescription(plain string, lists []leverList) string {
	parts := []string{}
	if plain != "" {
		parts = append(parts, plain)
	}
	for _, l := range lists {
		parts = append(parts, l.Text+"\n"+l.Content)
	}
	return strings.Join(parts, "\n")
}

// lastAfter returns the substring after the final occurrence of sep, or s if sep
// is absent. Mirrors JS `str.split(sep).pop()`.
func lastAfter(s, sep string) string {
	idx := strings.LastIndex(s, sep)
	if idx < 0 {
		return s
	}
	return s[idx+len(sep):]
}
