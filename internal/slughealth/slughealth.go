// Package slughealth ports scripts/validate-slugs.js: it checks configured ATS
// board slugs and writes the dashboard-compatible slug-health.json summary.
package slughealth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"job-search-automation/internal/discovery"
)

const (
	defaultTTL     = 23 * time.Hour
	defaultDelay   = 120 * time.Millisecond
	maxAttempts    = 3
	retryBaseDelay = 750 * time.Millisecond
	retryMaxDelay  = 2500 * time.Millisecond
)

// Config controls one slug-health run.
type Config struct {
	DataDir    string
	OutputPath string
	FilterATS  string
	BrokenOnly bool
	HTTPClient *http.Client
	Now        func() time.Time
	Log        *slog.Logger

	// Test hooks.
	Delay time.Duration
	TTL   time.Duration
}

// Summary is the JSON written for dashboard consumption.
type Summary struct {
	Timestamp  string            `json:"timestamp"`
	ProfileDir string            `json:"profileDir"`
	Total      Counts            `json:"total"`
	ByATS      map[string]Counts `json:"byAts"`
	Broken     []Issue           `json:"broken"`
	Blocked    []Issue           `json:"blocked"`
	Transient  []Issue           `json:"transient"`
	Skipped    bool              `json:"-"`
	Reason     string            `json:"-"`
}

// Counts aggregates validation categories.
type Counts struct {
	OK        int `json:"ok"`
	Empty     int `json:"empty"`
	Broken    int `json:"broken"`
	Blocked   int `json:"blocked"`
	Transient int `json:"transient"`
}

// Issue is one non-ok slug record.
type Issue struct {
	ATS      string `json:"ats"`
	Slug     string `json:"slug"`
	Note     string `json:"note"`
	Status   int    `json:"status"`
	Attempts int    `json:"attempts"`
	Category string `json:"-"`
}

type checkResult struct {
	Result   string
	Count    int
	Note     string
	Status   int
	Error    string
	URL      string
	Attempts int
}

// Run validates configured ATS slugs, respecting the 23-hour cache unless a
// FilterATS is set.
func Run(ctx context.Context, cfg Config) (Summary, error) {
	if cfg.DataDir == "" {
		cfg.DataDir = "data"
	}
	if cfg.OutputPath == "" {
		cfg.OutputPath = "slug-health.json"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Delay == 0 {
		cfg.Delay = defaultDelay
	}
	if cfg.TTL == 0 {
		cfg.TTL = defaultTTL
	}
	filter := strings.ToLower(strings.TrimSpace(cfg.FilterATS))

	if filter == "" {
		if fresh, err := freshCache(cfg.OutputPath, cfg.Now(), cfg.TTL); err == nil && fresh {
			return Summary{Skipped: true, Reason: "cache_fresh"}, nil
		}
	}

	companyCfg, err := discovery.LoadCompanyConfig(cfg.DataDir)
	if err != nil {
		return Summary{}, err
	}
	summary := Summary{
		Timestamp:  jsISOString(cfg.Now()),
		ProfileDir: cfg.DataDir,
		ByATS:      map[string]Counts{},
	}

	batches := []batch{
		stringBatch("Greenhouse", companyCfg.Greenhouse, checkGreenhouse),
		stringBatch("Lever", companyCfg.Lever, checkLever),
		stringBatch("Ashby", companyCfg.Ashby, checkAshby),
		stringBatch("Workable", companyCfg.Workable, checkWorkable),
		workdayBatch(companyCfg.Workday),
		stringBatch("Rippling", companyCfg.Rippling, checkRippling),
	}
	for _, b := range batches {
		if filter != "" && filter != strings.ToLower(b.name) {
			continue
		}
		counts, issues, err := runBatch(ctx, cfg, b)
		if err != nil {
			return Summary{}, err
		}
		summary.ByATS[b.name] = counts
		summary.Total.add(counts)
		for _, issue := range issues {
			switch issue.Category {
			case "blocked":
				summary.Blocked = append(summary.Blocked, issue)
			case "transient":
				summary.Transient = append(summary.Transient, issue)
			default:
				summary.Broken = append(summary.Broken, issue)
			}
		}
	}
	if err := writeSummary(cfg.OutputPath, summary); err != nil {
		return Summary{}, err
	}
	return summary, nil
}

func freshCache(path string, now time.Time, ttl time.Duration) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var prev struct {
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(raw, &prev); err != nil || prev.Timestamp == "" {
		return false, err
	}
	ts, err := time.Parse(time.RFC3339Nano, prev.Timestamp)
	if err != nil {
		return false, err
	}
	return now.Sub(ts) < ttl, nil
}

func writeSummary(path string, summary Summary) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o644)
}

func (c *Counts) add(other Counts) {
	c.OK += other.OK
	c.Empty += other.Empty
	c.Broken += other.Broken
	c.Blocked += other.Blocked
	c.Transient += other.Transient
}

type batch struct {
	name  string
	items []checkItem
}

type checkItem struct {
	label string
	value any
	fn    func(context.Context, Config, any) checkResult
}

func stringBatch(name string, values []string, fn func(context.Context, Config, any) checkResult) batch {
	items := make([]checkItem, 0, len(values))
	for _, v := range values {
		items = append(items, checkItem{label: v, value: v, fn: fn})
	}
	return batch{name: name, items: items}
}

func workdayBatch(values []discovery.WorkdayEntry) batch {
	items := make([]checkItem, 0, len(values))
	for _, v := range values {
		label := v.Label
		if label == "" {
			label = v.Sub
		}
		items = append(items, checkItem{label: label, value: v, fn: checkWorkday})
	}
	return batch{name: "Workday", items: items}
}

func runBatch(ctx context.Context, cfg Config, b batch) (Counts, []Issue, error) {
	var counts Counts
	var issues []Issue
	for _, item := range b.items {
		result := runCheckWithRetries(ctx, cfg, item)
		switch result.Result {
		case "ok":
			counts.OK++
		case "empty":
			counts.Empty++
		case "blocked":
			counts.Blocked++
			issues = append(issues, issueEntry(b.name, item.label, result))
		case "transient":
			counts.Transient++
			issues = append(issues, issueEntry(b.name, item.label, result))
		default:
			counts.Broken++
			issues = append(issues, issueEntry(b.name, item.label, result))
		}
		if cfg.Delay > 0 {
			select {
			case <-time.After(cfg.Delay):
			case <-ctx.Done():
				return Counts{}, nil, ctx.Err()
			}
		}
	}
	return counts, issues, nil
}

func runCheckWithRetries(ctx context.Context, cfg Config, item checkItem) checkResult {
	var latest checkResult
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		latest = item.fn(ctx, cfg, item.value)
		latest.Attempts = attempt
		shouldRetry := isRetryable(latest.Result) && attempt < maxAttempts
		if !shouldRetry {
			return latest
		}
		delay := retryBaseDelay * time.Duration(1<<(attempt-1))
		if latest.Status == http.StatusTooManyRequests || delay > retryMaxDelay {
			delay = retryMaxDelay
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return checkResult{Result: "transient", Note: ctx.Err().Error(), Error: ctx.Err().Error(), Attempts: attempt}
		}
	}
	return latest
}

func issueEntry(ats, slug string, result checkResult) Issue {
	return Issue{
		ATS:      ats,
		Slug:     slug,
		Note:     firstNonEmpty(result.Note, result.Result),
		Status:   result.Status,
		Attempts: result.Attempts,
		Category: result.Result,
	}
}

func isRetryable(result string) bool {
	return result == "blocked" || result == "transient"
}

func checkGreenhouse(ctx context.Context, cfg Config, v any) checkResult {
	slug := v.(string)
	u := "https://boards-api.greenhouse.io/v1/boards/" + slug + "/jobs"
	res := doJSON(ctx, cfg, http.MethodGet, u, nil)
	if res.Result != "" {
		return res.checkResult
	}
	var data struct {
		Jobs []any `json:"jobs"`
	}
	if err := json.Unmarshal(resBody(res), &data); err != nil {
		return checkResult{Result: "broken", Note: err.Error(), Status: res.Status, URL: u}
	}
	return countResult(len(data.Jobs), res.Status, u)
}

func checkLever(ctx context.Context, cfg Config, v any) checkResult {
	slug := v.(string)
	u := "https://api.lever.co/v0/postings/" + slug + "?mode=json"
	res := doJSON(ctx, cfg, http.MethodGet, u, nil)
	if res.Result != "" {
		return res.checkResult
	}
	var jobs []any
	if err := json.Unmarshal(resBody(res), &jobs); err != nil {
		return checkResult{Result: "broken", Note: err.Error(), Status: res.Status, URL: u}
	}
	return countResult(len(jobs), res.Status, u)
}

func checkAshby(ctx context.Context, cfg Config, v any) checkResult {
	slug := v.(string)
	u := "https://api.ashbyhq.com/posting-api/job-board/" + slug
	res := doJSON(ctx, cfg, http.MethodGet, u, nil)
	if res.Result != "" {
		return res.checkResult
	}
	var data struct {
		Jobs []any `json:"jobs"`
	}
	if err := json.Unmarshal(resBody(res), &data); err != nil {
		return checkResult{Result: "broken", Note: err.Error(), Status: res.Status, URL: u}
	}
	return countResult(len(data.Jobs), res.Status, u)
}

func checkWorkday(ctx context.Context, cfg Config, v any) checkResult {
	entry := v.(discovery.WorkdayEntry)
	u := fmt.Sprintf("https://%s.wd%d.myworkdayjobs.com/wday/cxs/%s/%s/jobs", entry.Sub, entry.WD, entry.Sub, entry.Board)
	body := bytes.NewBufferString(`{"limit":1,"offset":0,"searchText":""}`)
	res := doJSON(ctx, cfg, http.MethodPost, u, body)
	if res.Result != "" {
		return res.checkResult
	}
	var data struct {
		Total       *int  `json:"total"`
		JobPostings []any `json:"jobPostings"`
	}
	if err := json.Unmarshal(resBody(res), &data); err != nil {
		return checkResult{Result: "broken", Note: err.Error(), Status: res.Status, URL: u}
	}
	count := len(data.JobPostings)
	if data.Total != nil {
		count = *data.Total
	}
	return countResult(count, res.Status, u)
}

func checkWorkable(ctx context.Context, cfg Config, v any) checkResult {
	slug := v.(string)
	endpoints := []struct {
		method string
		url    string
		body   io.Reader
	}{
		{http.MethodGet, "https://www.workable.com/api/accounts/" + slug + "?details=true", nil},
		{http.MethodGet, "https://apply.workable.com/api/v1/widget/accounts/" + slug, nil},
		{http.MethodPost, "https://apply.workable.com/api/v3/accounts/" + slug + "/jobs", bytes.NewBufferString(`{"query":"","location":[],"department":[],"worktype":[],"remote":[]}`)},
	}
	var last checkResult
	for _, endpoint := range endpoints {
		res := doJSON(ctx, cfg, endpoint.method, endpoint.url, endpoint.body)
		if res.Result == "" {
			count := countWorkableJobs(resBody(res))
			return countResult(count, res.Status, endpoint.url)
		}
		last = res.checkResult
		if res.Result == "blocked" {
			return res.checkResult
		}
	}
	if last.Result == "" {
		last.Result = "broken"
	}
	return last
}

func checkRippling(ctx context.Context, cfg Config, v any) checkResult {
	slug := v.(string)
	u := "https://ats.rippling.com/" + slug + "/jobs"
	raw, res := doText(ctx, cfg, http.MethodGet, u, nil)
	if res.Result != "" {
		return res
	}
	match := regexp.MustCompile(`<script id="__NEXT_DATA__" type="application/json">([\s\S]*?)</script>`).FindStringSubmatch(raw)
	if len(match) < 2 {
		return checkResult{Result: "broken", Note: "Missing Rippling job data", Status: res.Status, URL: u}
	}
	var data any
	if err := json.Unmarshal([]byte(match[1]), &data); err != nil {
		return checkResult{Result: "broken", Note: "Invalid Rippling job data: " + err.Error(), Status: res.Status, URL: u}
	}
	count := countRipplingJobs(data)
	return countResult(count, res.Status, u)
}

type rawResult struct {
	checkResult
	body []byte
}

func doJSON(ctx context.Context, cfg Config, method, u string, body io.Reader) rawResult {
	raw, result := doRequest(ctx, cfg, method, u, body, "application/json")
	return rawResult{checkResult: result, body: raw}
}

func doText(ctx context.Context, cfg Config, method, u string, body io.Reader) (string, checkResult) {
	raw, result := doRequest(ctx, cfg, method, u, body, "text/html,application/xhtml+xml")
	return string(raw), result
}

func doRequest(ctx context.Context, cfg Config, method, u string, body io.Reader, accept string) ([]byte, checkResult) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, checkResult{Result: "broken", Note: err.Error(), Error: err.Error(), URL: u}
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", accept)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := cfg.HTTPClient.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			err = firstErr(err, ctx.Err())
		}
		result := classifyFailure(0, err.Error(), "", nil)
		return nil, checkResult{Result: result, Note: failureNote(0, err.Error(), result), Error: err.Error(), URL: u}
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if readErr != nil {
		result := classifyFailure(resp.StatusCode, readErr.Error(), "", nil)
		return raw, checkResult{Result: result, Note: failureNote(resp.StatusCode, readErr.Error(), result), Status: resp.StatusCode, Error: readErr.Error(), URL: u}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		headers := map[string]string{
			"server":     resp.Header.Get("server"),
			"via":        resp.Header.Get("via"),
			"cfRay":      resp.Header.Get("cf-ray"),
			"retryAfter": resp.Header.Get("retry-after"),
		}
		result := classifyFailure(resp.StatusCode, "", string(raw), headers)
		return raw, checkResult{Result: result, Note: failureNote(resp.StatusCode, "", result), Status: resp.StatusCode, URL: u}
	}
	return raw, checkResult{Status: resp.StatusCode, URL: u}
}

func resBody(res rawResult) []byte { return res.body }

func classifyFailure(status int, errText, text string, headers map[string]string) string {
	if status == http.StatusNotFound || status == 422 {
		return "broken"
	}
	if isBlockedResponse(status, text, headers) {
		return "blocked"
	}
	if status >= 500 {
		return "transient"
	}
	normalized := strings.ToLower(errText)
	if status == 0 || regexp.MustCompile(`timeout|abort|dns|enotfound|eai_again|etimedout|econnreset|fetch failed|network`).MatchString(normalized) {
		return "transient"
	}
	return "broken"
}

func isBlockedResponse(status int, text string, headers map[string]string) bool {
	var parts []string
	parts = append(parts, text)
	for _, v := range headers {
		parts = append(parts, v)
	}
	haystack := strings.ToLower(strings.Join(parts, " "))
	return status == http.StatusTooManyRequests ||
		regexp.MustCompile(`cloudflare|cf-ray|rate.?limit|too many requests|captcha|bot.?detect|access denied|akamai|perimeterx|datadome`).MatchString(haystack)
}

func failureNote(status int, errText, category string) string {
	if status != 0 {
		return fmt.Sprintf("HTTP %d", status)
	}
	if errText != "" {
		return errText
	}
	return category
}

func countResult(count, status int, u string) checkResult {
	result := "empty"
	if count > 0 {
		result = "ok"
	}
	return checkResult{Result: result, Count: count, Status: status, URL: u}
}

func countWorkableJobs(raw []byte) int {
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		return 0
	}
	seen := map[string]bool{}
	return countJobObjects(data, seen)
}

func countJobObjects(value any, seen map[string]bool) int {
	switch v := value.(type) {
	case []any:
		total := 0
		for _, item := range v {
			total += countJobObjects(item, seen)
		}
		return total
	case map[string]any:
		if looksLikeWorkableJob(v) {
			key := firstNonEmpty(asString(v["shortcode"]), asString(v["code"]), asString(v["id"]), asString(v["url"]))
			if key == "" {
				key = fmt.Sprintf("%v", v)
			}
			if seen[key] {
				return 0
			}
			seen[key] = true
			return 1
		}
		total := 0
		for _, nested := range v {
			total += countJobObjects(nested, seen)
		}
		return total
	default:
		return 0
	}
}

func looksLikeWorkableJob(v map[string]any) bool {
	title := firstNonEmpty(asString(v["title"]), asString(v["full_title"]))
	id := firstNonEmpty(asString(v["shortcode"]), asString(v["code"]), asString(v["id"]), asString(v["url"]), asString(v["application_url"]))
	return title != "" && id != ""
}

func countRipplingJobs(data any) int {
	root, ok := data.(map[string]any)
	if !ok {
		return 0
	}
	props, _ := root["props"].(map[string]any)
	pageProps, _ := props["pageProps"].(map[string]any)
	state, _ := pageProps["dehydratedState"].(map[string]any)
	queries, _ := state["queries"].([]any)
	for _, q := range queries {
		qm, _ := q.(map[string]any)
		key, _ := qm["queryKey"].([]any)
		if len(key) < 3 || asString(key[2]) != "job-posts" {
			continue
		}
		state, _ := qm["state"].(map[string]any)
		jobsData, _ := state["data"].(map[string]any)
		if n := intFromAny(jobsData["totalItems"]); n > 0 {
			return n
		}
		if n := intFromAny(jobsData["total"]); n > 0 {
			return n
		}
		if items, ok := jobsData["items"].([]any); ok {
			return len(items)
		}
	}
	return 0
}

func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

func asString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	default:
		return ""
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func jsISOString(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
