package closedcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// PostFetcher performs an HTTP POST with a JSON body. Like Fetcher it returns
// nil for network failures so callers skip instead of closing on uncertainty.
type PostFetcher func(ctx context.Context, rawURL string, body []byte) (*HTTPResult, error)

// DefaultPostFetcher returns a PostFetcher over the given client (nil gets a
// default with the package timeout). It sends the headers Workday's CXS API
// needs: a concrete accept-language, because some Workday WAFs 500 on a bare
// or wildcard value (see the 2026-07-02 accept-language incident).
func DefaultPostFetcher(client *http.Client) PostFetcher {
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	return func(ctx context.Context, rawURL string, body []byte) (*HTTPResult, error) {
		ctx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
		if err != nil {
			return nil, nil
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; job-search-bot/1.0)")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Accept-Language", "en-US")
		res, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, nil
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		return &HTTPResult{StatusCode: res.StatusCode, Body: data}, nil
	}
}

// workdayPosting is what CheckWorkdayPosting parses out of a stored posting
// URL (https://<sub>.wd<N>.myworkdayjobs.com/<lang>/<board>/job/<loc>/<slug>_<req>).
type workdayPosting struct {
	searchURL string // POST /wday/cxs/<sub>/<board>/jobs
	path      string // /job/<loc>/<slug>_<req>, the externalPath to match
	reqID     string // <req>, the search text
}

func parseWorkdayPosting(rawURL string) *workdayPosting {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if !strings.HasSuffix(host, ".myworkdayjobs.com") {
		return nil
	}
	sub := strings.SplitN(host, ".", 2)[0]
	if sub == "" {
		return nil
	}
	parts := pathParts(u.Path)
	if len(parts) < 5 || !strings.EqualFold(parts[2], "job") {
		return nil
	}
	last := parts[len(parts)-1]
	i := strings.LastIndex(last, "_")
	if i <= 0 || i == len(last)-1 {
		return nil
	}
	return &workdayPosting{
		searchURL: "https://" + host + "/wday/cxs/" + sub + "/" + parts[1] + "/jobs",
		path:      "/" + strings.Join(parts[2:], "/"),
		reqID:     last[i+1:],
	}
}

// workdayVariantSuffix is a short numeric posting-variant tail on a req id
// (Staff-Software-Engineer_R0134153-1). Only 1-2 digits qualify: longer digit
// runs are part of the id itself (nike's R-86901).
var workdayVariantSuffix = regexp.MustCompile(`-\d{1,2}$`)

// CheckWorkdayPosting asks the posting's own board whether it is still live,
// via the CXS search endpoint (the /job/ detail endpoints are WAF-blocked on
// many boards; search is served plainly). Verdicts follow CheckJob: "open"
// when a search hit's externalPath matches the posting URL (case-insensitive,
// so case-drifted twins still count), "closed" when the search answered
// cleanly without a match, "" when nothing can be concluded (non-Workday or
// unparseable URL, non-200, garbage body). The search runs on the full req id
// and once more with a short variant suffix stripped, since Workday's search
// indexes the base req.
func CheckWorkdayPosting(ctx context.Context, post PostFetcher, postingURL string) (string, error) {
	p := parseWorkdayPosting(postingURL)
	if p == nil {
		return "", nil
	}
	terms := []string{p.reqID}
	if core := workdayVariantSuffix.ReplaceAllString(p.reqID, ""); core != p.reqID && core != "" {
		terms = append(terms, core)
	}
	for _, term := range terms {
		body, err := json.Marshal(map[string]any{
			"searchText": term, "limit": 20, "offset": 0, "appliedFacets": map[string]any{},
		})
		if err != nil {
			return "", nil
		}
		res, err := post(ctx, p.searchURL, body)
		if err != nil || res == nil {
			return "", err
		}
		if res.StatusCode != http.StatusOK {
			return "", nil
		}
		var out struct {
			Total       int `json:"total"`
			JobPostings []struct {
				ExternalPath string `json:"externalPath"`
			} `json:"jobPostings"`
		}
		if json.Unmarshal(res.Body, &out) != nil {
			return "", nil
		}
		for _, jp := range out.JobPostings {
			if strings.EqualFold(jp.ExternalPath, p.path) {
				return "open", nil
			}
		}
		if out.Total > len(out.JobPostings) {
			// More hits than one page holds means the term matched broadly;
			// absence from the visible page proves nothing.
			return "", nil
		}
	}
	return "closed", nil
}
