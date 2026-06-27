// Package scorer ports the Node Gemini scorer (scorer.js, lib/gemini.js) to Go.
// It scores job listings against the candidate's resume by calling the Gemini
// API over HTTP.
//
// One deliberate difference from the Node version: the Node scorer coordinated
// several processes through a filesystem lock (logs/gemini-rate-limit/). The Go
// backend paces calls with a process-global limiter shared per API key (see
// sharedLimiter): every Client built with the same key — background cron and the
// many short-lived per-request dashboard scorers alike — coordinates through one
// pacer, so aggregate RPM against a shared key stays bounded no matter how many
// tenants or requests are in flight. Grants are round-robin across tenants so one
// tenant's bulk run can't starve another's.
package scorer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"job-search-automation/internal/metrics"
)

// Model is the scoring model; it also keys the api_usage table.
const Model = "gemini-3.1-flash-lite"

const geminiURLTemplate = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent"

// These mirror the constants in config/constants.js that the Node side did not
// expose as env vars.
const (
	maxRetries            = 5
	defaultMaxOutputToken = 600
	retryBaseDelay        = 5 * time.Second
	delay429              = 30 * time.Second
)

// UsageRecorder records a Gemini call against the daily api_usage tally. The
// repository satisfies it; tracking failures must never break scoring.
// RecordHostAPICall additionally tallies calls made on the shared host key
// against a global (cross-tenant) bucket so the host key can't be pushed past its
// real provider quota by any one tenant.
type UsageRecorder interface {
	RecordAPICall(model string) error
	RecordHostAPICall(model string) error
}

// TokenUsageRecorder is implemented by usage stores that persist Gemini
// usageMetadata token counters. It is intentionally optional so tests and
// alternate callers that only care about call quotas can keep implementing the
// smaller UsageRecorder surface.
type TokenUsageRecorder interface {
	RecordAPITokens(model string, promptTokens, cachedPromptTokens, candidateTokens, totalTokens int) error
	RecordHostAPITokens(model string, promptTokens, cachedPromptTokens, candidateTokens, totalTokens int) error
}

// Client is a Gemini HTTP client with built-in pacing and retry.
type Client struct {
	apiKey  string
	model   string // model id used for the endpoint and api_usage accounting
	baseURL string // overridable in tests; defaults to the public Gemini endpoint
	http    *http.Client
	limiter *sharedLimiter
	usage   UsageRecorder
	log     *slog.Logger

	// tenant scopes this client's calls in the shared limiter's round-robin so
	// one tenant's bulk run can't starve another. Empty means the default bucket.
	tenant string
	// hostKey is true when apiKey is the shared host key, so successful calls are
	// also tallied against the global host quota.
	hostKey bool

	// retry delays, fields so tests can shrink them.
	retry429  time.Duration
	retryBase time.Duration
}

// NewClient builds a Gemini client for the default scoring model (Model).
// rateDelay is the minimum spacing between calls; usage may be nil (calls are
// then not tallied). The pacing limiter is shared process-wide by every client
// built with the same (apiKey, model).
func NewClient(apiKey string, rateDelay time.Duration, usage UsageRecorder, log *slog.Logger) *Client {
	return NewClientForModel(apiKey, Model, rateDelay, usage, log)
}

// NewClientForModel builds a Gemini client pinned to a specific model id. Each
// model gets its own endpoint, its own api_usage tally (the table is keyed by
// model), and — because the limiter is keyed by (apiKey, model) — its own RPM
// pacing.
func NewClientForModel(apiKey, model string, rateDelay time.Duration, usage UsageRecorder, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	if model == "" {
		model = Model
	}
	return &Client{
		apiKey:    apiKey,
		model:     model,
		baseURL:   fmt.Sprintf(geminiURLTemplate, model),
		http:      &http.Client{Timeout: 60 * time.Second},
		limiter:   limiterFor(apiKey, model, rateDelay),
		usage:     usage,
		log:       log,
		retry429:  delay429,
		retryBase: retryBaseDelay,
	}
}

// WithTenant scopes this client's calls to a tenant in the shared limiter's
// round-robin scheduling. Returns the client for chaining.
func (c *Client) WithTenant(tenant string) *Client {
	c.tenant = tenant
	return c
}

// WithHostKey marks whether this client uses the shared host key, so successful
// calls are also counted against the global host quota. Returns the client for
// chaining.
func (c *Client) WithHostKey(host bool) *Client {
	c.hostKey = host
	return c
}

type geminiRequest struct {
	Contents         []geminiContent  `json:"contents"`
	GenerationConfig generationConfig `json:"generationConfig"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type generationConfig struct {
	MaxOutputTokens int `json:"maxOutputTokens"`
	// Temperature pins sampling. It is set (to ScoreTemperature) only on the scoring
	// paths so the same job gets a stable score across runs; left nil elsewhere so
	// discovery and ATS resolution keep the model default, where variation helps.
	Temperature *float64 `json:"temperature,omitempty"`
	// ResponseMimeType + ResponseSchema enable Gemini structured output. They are
	// set only for batch scoring so the model returns a parseable JSON array
	// instead of free text; single-job scoring leaves them empty.
	ResponseMimeType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
}

// ScoreTemperature pins both scoring paths to deterministic output. Scoring with
// the model default (~1.0) made the same job's score wander 2-4 points between
// runs, which undercut the two-stage threshold; 0 makes scores reproducible.
const ScoreTemperature = 0.0

func tempPtr(f float64) *float64 { return &f }

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	UsageMetadata geminiUsageMetadata `json:"usageMetadata"`
	Error         struct {
		Message string `json:"message"`
	} `json:"error"`
}

type geminiUsageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
}

// CallGemini sends a prompt and returns the model's text, pacing and retrying
// like lib/gemini.js: 429s wait a fixed delay, 5xx back off exponentially, and
// up to maxRetries retries are attempted before the last error is returned.
func (c *Client) CallGemini(ctx context.Context, prompt string, maxTokens int) (string, error) {
	if maxTokens <= 0 {
		maxTokens = defaultMaxOutputToken
	}
	return c.generate(ctx, "generate", prompt, generationConfig{MaxOutputTokens: maxTokens})
}

// CallGeminiScore is single-job scoring's entry point: like CallGemini but pinned
// to ScoreTemperature for reproducible scores. It is kept separate from CallGemini
// so discovery and ATS resolution, which share CallGemini, keep the model default.
func (c *Client) CallGeminiScore(ctx context.Context, prompt string, maxTokens int) (string, error) {
	if maxTokens <= 0 {
		maxTokens = defaultMaxOutputToken
	}
	return c.generate(ctx, "score_single", prompt, generationConfig{
		MaxOutputTokens: maxTokens,
		Temperature:     tempPtr(ScoreTemperature),
	})
}

// CallGeminiJSON is CallGemini with Gemini structured output turned on: the model
// is constrained to schema and returns a JSON string. Used by batch scoring so a
// single call yields one parseable array for many jobs; pinned to ScoreTemperature
// like single-job scoring so batch and single agree.
func (c *Client) CallGeminiJSON(ctx context.Context, prompt string, maxTokens int, schema json.RawMessage) (string, error) {
	if maxTokens <= 0 {
		maxTokens = defaultMaxOutputToken
	}
	return c.generate(ctx, "score_batch", prompt, generationConfig{
		MaxOutputTokens:  maxTokens,
		Temperature:      tempPtr(ScoreTemperature),
		ResponseMimeType: "application/json",
		ResponseSchema:   schema,
	})
}

// generate runs the shared pace/retry loop for any generation config.
func (c *Client) generate(ctx context.Context, operation, prompt string, gc generationConfig) (string, error) {
	if c.apiKey == "" {
		return "", errors.New("GEMINI_API_KEY environment variable is not set")
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := c.limiter.acquire(ctx, c.tenant); err != nil {
			return "", err
		}

		start := time.Now()
		text, usage, status, err := c.doRequest(ctx, prompt, gc)
		result := "ok"
		if err != nil {
			result = "error"
		}
		metrics.ObserveGeminiRequest(operation, result, statusLabel(status), time.Since(start))
		if err == nil {
			metrics.ObserveGeminiTokens(
				operation,
				usage.PromptTokenCount,
				usage.CachedContentTokenCount,
				usage.CandidatesTokenCount,
				usage.TotalTokenCount,
			)
			if c.usage != nil {
				if recErr := c.usage.RecordAPICall(c.model); recErr != nil {
					c.log.Warn("api usage tracking failed", "error", recErr)
				}
				if tokenUsage, ok := c.usage.(TokenUsageRecorder); ok {
					if recErr := tokenUsage.RecordAPITokens(c.model,
						usage.PromptTokenCount,
						usage.CachedContentTokenCount,
						usage.CandidatesTokenCount,
						usage.TotalTokenCount,
					); recErr != nil {
						c.log.Warn("api token usage tracking failed", "error", recErr)
					}
				}
				if c.hostKey {
					if recErr := c.usage.RecordHostAPICall(c.model); recErr != nil {
						c.log.Warn("host api usage tracking failed", "error", recErr)
					}
					if tokenUsage, ok := c.usage.(TokenUsageRecorder); ok {
						if recErr := tokenUsage.RecordHostAPITokens(c.model,
							usage.PromptTokenCount,
							usage.CachedContentTokenCount,
							usage.CandidatesTokenCount,
							usage.TotalTokenCount,
						); recErr != nil {
							c.log.Warn("host api token usage tracking failed", "error", recErr)
						}
					}
				}
			}
			return text, nil
		}
		lastErr = err

		retryable := status == http.StatusTooManyRequests || status >= 500
		if !retryable || attempt == maxRetries {
			break
		}

		delay := c.retryBase * (1 << attempt)
		if status == http.StatusTooManyRequests {
			delay = c.retry429
		}
		c.log.Warn("gemini retry", "status", status, "delay", delay.String(), "attempt", attempt+1)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "", lastErr
}

func statusLabel(status int) string {
	if status == 0 {
		return "transport"
	}
	return fmt.Sprintf("%d", status)
}

// doRequest performs a single Gemini call. It returns the parsed text on success,
// or the HTTP status (0 on transport error) and an error otherwise.
func (c *Client) doRequest(ctx context.Context, prompt string, gc generationConfig) (string, geminiUsageMetadata, int, error) {
	body, err := json.Marshal(geminiRequest{
		Contents:         []geminiContent{{Parts: []geminiPart{{Text: prompt}}}},
		GenerationConfig: gc,
	})
	if err != nil {
		return "", geminiUsageMetadata{}, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return "", geminiUsageMetadata{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", geminiUsageMetadata{}, 0, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", geminiUsageMetadata{}, resp.StatusCode, err
	}

	var parsed geminiResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// A non-JSON body on an error status still needs to surface the status.
		if resp.StatusCode != http.StatusOK {
			return "", geminiUsageMetadata{}, resp.StatusCode, fmt.Errorf("gemini error %d", resp.StatusCode)
		}
		return "", geminiUsageMetadata{}, resp.StatusCode, err
	}

	if resp.StatusCode != http.StatusOK {
		msg := parsed.Error.Message
		if msg == "" {
			msg = fmt.Sprintf("gemini error %d", resp.StatusCode)
		}
		return "", geminiUsageMetadata{}, resp.StatusCode, errors.New(msg)
	}

	var text string
	if len(parsed.Candidates) > 0 && len(parsed.Candidates[0].Content.Parts) > 0 {
		text = parsed.Candidates[0].Content.Parts[0].Text
	}
	return strings.TrimSpace(text), parsed.UsageMetadata, resp.StatusCode, nil
}

// limiters holds one sharedLimiter per API key, keyed by a hash of the key so the
// plaintext key isn't a map key. Every Client built with the same key shares its
// limiter, making pacing process-global instead of per-instance.
var (
	limiterMu sync.Mutex
	limiters  = map[string]*sharedLimiter{}
)

// limiterFor returns the process-global limiter for (apiKey, model), creating it
// on first use. The first caller fixes the delay (uniform in practice). The model
// is part of the key because Gemini RPM limits are per-model, so models sharing
// one API key pace independently rather than collapsing onto one limiter. An empty
// key gets a no-op limiter (delay 0) so the "no key configured" path keeps its old
// behavior.
func limiterFor(apiKey, model string, delay time.Duration) *sharedLimiter {
	if apiKey == "" {
		return &sharedLimiter{delay: 0}
	}
	sum := sha256.Sum256([]byte(apiKey + "\x00" + model))
	id := hex.EncodeToString(sum[:])

	limiterMu.Lock()
	defer limiterMu.Unlock()
	l := limiters[id]
	if l == nil {
		l = &sharedLimiter{delay: delay, queues: map[string][]*waiter{}}
		limiters[id] = l
	}
	return l
}

// waiter is one pending acquire(); the dispatcher closes ready to grant the slot.
type waiter struct {
	ready     chan struct{}
	cancelled bool // set under sharedLimiter.mu when the caller's ctx is done
}

// sharedLimiter spaces Gemini calls for one API key at least `delay` apart and
// grants those slots round-robin across tenants. The pacing (nextSlot/delay)
// mirrors the slot-reservation in lib/gemini.js; the round-robin is what keeps a
// tenant submitting thousands of jobs from monopolizing the slots ahead of a
// tenant submitting one. A single lazily-started dispatcher goroutine owns slot
// reservation and grants; it self-terminates when no waiters remain.
type sharedLimiter struct {
	mu          sync.Mutex
	delay       time.Duration
	nextSlot    time.Time
	queues      map[string][]*waiter // tenant -> FIFO of pending waiters
	rr          []string             // tenants in round-robin order
	cursor      int                  // next rr index to serve
	dispatching bool                 // a dispatch goroutine is running
}

// acquire blocks until this caller is granted the next slot for its tenant, or
// the context is done. delay <= 0 makes it a no-op (used for the empty-key client
// and tests).
func (l *sharedLimiter) acquire(ctx context.Context, tenant string) error {
	if l.delay <= 0 {
		return nil
	}
	w := &waiter{ready: make(chan struct{})}

	l.mu.Lock()
	if l.queues == nil {
		l.queues = map[string][]*waiter{}
	}
	if _, ok := l.queues[tenant]; !ok {
		l.rr = append(l.rr, tenant)
	}
	l.queues[tenant] = append(l.queues[tenant], w)
	if !l.dispatching {
		l.dispatching = true
		go l.dispatch()
	}
	l.mu.Unlock()

	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		// Prefer an already-granted slot if the dispatcher won the race.
		l.mu.Lock()
		select {
		case <-w.ready:
			l.mu.Unlock()
			return nil
		default:
			w.cancelled = true
			l.mu.Unlock()
			return ctx.Err()
		}
	}
}

// dispatch reserves and grants one slot at a time in round-robin tenant order,
// sleeping between slots to honor delay. It exits when no waiters remain.
func (l *sharedLimiter) dispatch() {
	for {
		l.mu.Lock()
		w, ok := l.nextWaiterLocked()
		if !ok {
			l.dispatching = false
			l.mu.Unlock()
			return
		}
		now := time.Now()
		slot := now
		if l.nextSlot.After(now) {
			slot = l.nextSlot
		}
		l.nextSlot = slot.Add(l.delay)
		l.mu.Unlock()

		if d := time.Until(slot); d > 0 {
			time.Sleep(d)
		}

		l.mu.Lock()
		if w.cancelled {
			// Caller gave up after we reserved the slot; skip the grant.
			l.mu.Unlock()
			continue
		}
		close(w.ready)
		l.mu.Unlock()
	}
}

// nextWaiterLocked pops the next waiter to serve in round-robin tenant order,
// discarding cancelled waiters and drained tenants without consuming a slot.
// Caller holds l.mu. Returns false when no live waiter remains.
func (l *sharedLimiter) nextWaiterLocked() (*waiter, bool) {
	for len(l.rr) > 0 {
		if l.cursor >= len(l.rr) {
			l.cursor = 0
		}
		tenant := l.rr[l.cursor]
		q := l.queues[tenant]

		// Drop leading cancelled waiters; they don't consume a slot.
		for len(q) > 0 && q[0].cancelled {
			q = q[1:]
		}
		if len(q) == 0 {
			// Tenant drained: remove it and re-check the same cursor position.
			delete(l.queues, tenant)
			l.rr = append(l.rr[:l.cursor], l.rr[l.cursor+1:]...)
			continue
		}

		w := q[0]
		l.queues[tenant] = q[1:]
		l.cursor++ // advance so the next grant goes to the next tenant
		return w, true
	}
	return nil, false
}
