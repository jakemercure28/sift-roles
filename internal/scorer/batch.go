package scorer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"job-search-automation/internal/metrics"
)

// ErrQuotaExhausted marks a batch submit rejected with HTTP 429. The batch
// path runs on the paid overflow key only (no failover), and a 429 there is
// usually billing exhaustion that no in-process backoff will outlast, so the
// client fails fast and the orchestrator imposes a submit cooldown instead of
// re-attempting every tick. Test with errors.Is.
var ErrQuotaExhausted = errors.New("gemini batch quota exhausted")

// BatchModel keys api_usage rows for batch-scored jobs. It is deliberately NOT
// the sync Model string: daily quotas and pacing are keyed by model, so batch
// spend must never eat the sync path's budget (and vice versa).
const BatchModel = Model + "-batch"

// geminiAPIRoot is the v1beta root the batch endpoints hang off. Unlike the
// sync client's baseURL (a full :generateContent URL), the batch client needs
// the root because it builds two URLs: submit (models/{model}:batchGenerateContent)
// and poll (/{batchName}).
const geminiAPIRoot = "https://generativelanguage.googleapis.com/v1beta"

// Batch states, normalized from the wire values. Google's docs disagree on the
// enum prefix (JOB_STATE_* in the guide, BATCH_STATE_* in the REST reference),
// so GetScoreBatch matches by suffix and returns these bare forms.
const (
	BatchStatePending   = "PENDING"
	BatchStateRunning   = "RUNNING"
	BatchStateSucceeded = "SUCCEEDED"
	BatchStateFailed    = "FAILED"
	BatchStateCancelled = "CANCELLED"
	BatchStateExpired   = "EXPIRED"
	BatchStateUnknown   = "UNKNOWN"
)

// BatchDone reports whether state is terminal: the batch will never make more
// progress and its DB record can be closed.
func BatchDone(state string) bool {
	switch state {
	case BatchStateSucceeded, BatchStateFailed, BatchStateCancelled, BatchStateExpired:
		return true
	}
	return false
}

// BatchClient submits and polls Gemini Batch API scoring jobs. Batch runs on
// the paid overflow key only (the free tier has no batch quota) at a flat 50%
// discount, and the API is asynchronous with a ~24h turnaround target, so
// unlike Client there is no RPM pacing, no failover, and no shared limiter.
type BatchClient struct {
	apiKey string
	model  string
	root   string // overridable in tests; defaults to geminiAPIRoot
	http   *http.Client
	log    *slog.Logger

	// thinking is the thinkingLevel for the batched scoring calls, mirroring
	// the sync path (SCORING_THINKING_LEVEL). Empty omits the field; see
	// WithThinkingLevel on Client for why empty is the right default.
	thinking string

	// retryBase is the submit retry backoff base, a field so tests can shrink it.
	retryBase time.Duration
}

// NewBatchClient builds a batch client on the paid overflow key for the
// default scoring model. log may be nil.
func NewBatchClient(overflowKey string, log *slog.Logger) *BatchClient {
	if log == nil {
		log = slog.Default()
	}
	return &BatchClient{
		apiKey:    overflowKey,
		model:     Model,
		root:      geminiAPIRoot,
		http:      &http.Client{Timeout: 120 * time.Second},
		log:       log,
		retryBase: retryBaseDelay,
	}
}

// WithThinkingLevel sets the thinkingConfig.thinkingLevel for the batched
// scoring calls, mirroring the sync scoring path. Empty (the default) omits
// the field. Returns the client for chaining.
func (c *BatchClient) WithThinkingLevel(level string) *BatchClient {
	c.thinking = strings.TrimSpace(level)
	return c
}

// BatchItem is one job's scoring request within a submitted batch. Key is the
// caller's correlation id (the job id); it round-trips through the API's
// per-request metadata and comes back on the matching BatchResult.
type BatchItem struct {
	Key    string
	Prompt string
}

// BatchStatus is a poll snapshot of a submitted batch.
type BatchStatus struct {
	Name  string
	State string // one of the BatchState* constants
	// Err is the batch-level error message, set when the whole batch failed.
	Err string
	// Results holds the per-item outcomes, populated only once the batch
	// succeeded.
	Results []BatchResult
}

// BatchResult is one item's outcome within a finished batch. Exactly one of
// Text or Err is meaningful. Token counts come from the item's usageMetadata
// so the caller can ledger the spend.
type BatchResult struct {
	Key  string
	Text string
	Err  string

	PromptTokens    int
	CachedTokens    int
	CandidateTokens int
	ThoughtTokens   int
	TotalTokens     int
}

// batchSubmitRequest is the :batchGenerateContent body. Field names are the
// canonical proto3 camelCase (the REST docs render snake_case; the API accepts
// both, and camelCase matches the rest of this package).
type batchSubmitRequest struct {
	Batch struct {
		DisplayName string `json:"displayName"`
		InputConfig struct {
			Requests struct {
				Requests []batchInlineRequest `json:"requests"`
			} `json:"requests"`
		} `json:"inputConfig"`
	} `json:"batch"`
}

type batchInlineRequest struct {
	Request  geminiRequest     `json:"request"`
	Metadata map[string]string `json:"metadata"`
}

// SubmitScoreBatch submits items as one inline batch and returns the server's
// batch name (e.g. "batches/abc123"), the handle every later poll uses. The
// scoring generationConfig matches the sync path exactly (temperature,
// maxOutputTokens, thinking) so a batch-scored job is indistinguishable from a
// sync-scored one. Retries transport errors and 5xx like the sync client;
// any 4xx fails immediately, with 429 wrapped in ErrQuotaExhausted so the
// orchestrator can cool down instead of retrying a quota that will not clear.
func (c *BatchClient) SubmitScoreBatch(ctx context.Context, displayName string, items []BatchItem) (string, error) {
	if c.apiKey == "" {
		return "", errors.New("batch scoring requires the overflow API key")
	}
	if len(items) == 0 {
		return "", errors.New("empty batch")
	}

	var body batchSubmitRequest
	body.Batch.DisplayName = displayName
	reqs := make([]batchInlineRequest, 0, len(items))
	for _, it := range items {
		gc := generationConfig{
			MaxOutputTokens: scoreMaxOutputToken,
			Temperature:     tempPtr(ScoreTemperature),
		}
		if c.thinking != "" {
			gc.ThinkingConfig = &thinkingConfig{ThinkingLevel: c.thinking}
		}
		reqs = append(reqs, batchInlineRequest{
			Request: geminiRequest{
				Contents:         []geminiContent{{Parts: []geminiPart{{Text: it.Prompt}}}},
				GenerationConfig: gc,
			},
			Metadata: map[string]string{"key": it.Key},
		})
	}
	body.Batch.InputConfig.Requests.Requests = reqs

	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/models/%s:batchGenerateContent", c.root, c.model)

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		name, status, err := c.doSubmit(ctx, url, payload)
		if err == nil {
			return name, nil
		}
		lastErr = err
		// 429 means quota, and on this key quota does not come back in a
		// retry window: surface the typed error at once.
		if status == http.StatusTooManyRequests {
			return "", fmt.Errorf("%w: %v", ErrQuotaExhausted, err)
		}
		// Non-retryable: any other 4xx means the request itself is wrong
		// and retrying can't fix it.
		if status >= 400 && status < 500 {
			return "", err
		}
		if attempt == maxRetries {
			break
		}
		delay := c.retryBase * time.Duration(attempt)
		c.log.Warn("batch submit failed; retrying", "attempt", attempt, "status", status, "error", err)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(delay):
		}
	}
	return "", lastErr
}

func (c *BatchClient) doSubmit(ctx context.Context, url string, payload []byte) (string, int, error) {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		metrics.ObserveGeminiRequest("score_batch_submit", "error", "transport", "overflow", time.Since(start))
		return "", 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		metrics.ObserveGeminiRequest("score_batch_submit", "error", statusLabel(resp.StatusCode), "overflow", time.Since(start))
		return "", resp.StatusCode, err
	}

	var parsed struct {
		Name  string `json:"name"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil && resp.StatusCode == http.StatusOK {
		metrics.ObserveGeminiRequest("score_batch_submit", "error", statusLabel(resp.StatusCode), "overflow", time.Since(start))
		return "", resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := parsed.Error.Message
		if msg == "" {
			msg = fmt.Sprintf("gemini batch error %d", resp.StatusCode)
		}
		metrics.ObserveGeminiRequest("score_batch_submit", "error", statusLabel(resp.StatusCode), "overflow", time.Since(start))
		return "", resp.StatusCode, errors.New(msg)
	}
	if parsed.Name == "" {
		metrics.ObserveGeminiRequest("score_batch_submit", "error", statusLabel(resp.StatusCode), "overflow", time.Since(start))
		return "", resp.StatusCode, errors.New("batch submit response has no name")
	}
	metrics.ObserveGeminiRequest("score_batch_submit", "ok", statusLabel(resp.StatusCode), "overflow", time.Since(start))
	return parsed.Name, resp.StatusCode, nil
}

// CancelScoreBatch asks Gemini to cancel a submitted batch. Single-shot
// best-effort (no retries): the caller has already closed its ledger row, so
// a failed cancel only means the upstream batch runs to an ignored completion.
func (c *BatchClient) CancelScoreBatch(ctx context.Context, name string) error {
	start := time.Now()
	url := c.root + "/" + strings.TrimPrefix(name, "/") + ":cancel"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		metrics.ObserveGeminiRequest("score_batch_cancel", "error", "transport", "overflow", time.Since(start))
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var parsed struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := ""
		if json.Unmarshal(raw, &parsed) == nil {
			msg = parsed.Error.Message
		}
		if msg == "" {
			msg = fmt.Sprintf("gemini batch error %d", resp.StatusCode)
		}
		metrics.ObserveGeminiRequest("score_batch_cancel", "error", statusLabel(resp.StatusCode), "overflow", time.Since(start))
		return errors.New(msg)
	}
	metrics.ObserveGeminiRequest("score_batch_cancel", "ok", statusLabel(resp.StatusCode), "overflow", time.Since(start))
	return nil
}

// batchInlined is one wire-format inlined response. Metadata echoes the
// submit-time {"key": jobID}.
type batchInlined struct {
	Metadata struct {
		Key string `json:"key"`
	} `json:"metadata"`
	Response *geminiResponse `json:"response"`
	Error    *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// inlinedList tolerates both wire encodings of an inlined-responses slot: a
// bare JSON array, and the InlinedResponses message wrapper
// {"inlinedResponses": [...]}. The live API returns the wrapper (verified
// 2026-07-10 against a real GenerateContentBatchOutput: the finished
// Operation carries response.inlinedResponses.inlinedResponses), while the
// docs show the bare array, so every slot must accept either.
type inlinedList []batchInlined

func (l *inlinedList) UnmarshalJSON(b []byte) error {
	var arr []batchInlined
	if err := json.Unmarshal(b, &arr); err == nil {
		*l = arr
		return nil
	}
	var wrap struct {
		InlinedResponses []batchInlined `json:"inlinedResponses"`
	}
	if err := json.Unmarshal(b, &wrap); err != nil {
		return err
	}
	*l = wrap.InlinedResponses
	return nil
}

// batchPollResponse tolerates every documented and observed shape of the
// batch resource. The guide shows a flat Batch ({state,
// dest.inlinedResponses}); the REST reference shows a long-running Operation
// ({metadata.state, response.inlinedResponses} or {response.state,
// response.output.inlinedResponses}); the live API (see inlinedList) wraps
// each inlined slot in an extra message object. All paths are declared and
// the accessors below take the first populated one.
type batchPollResponse struct {
	Name     string `json:"name"`
	Done     bool   `json:"done"`
	State    string `json:"state"`
	Metadata struct {
		State string `json:"state"`
	} `json:"metadata"`
	Response struct {
		State            string      `json:"state"`
		InlinedResponses inlinedList `json:"inlinedResponses"`
		Output           struct {
			InlinedResponses inlinedList `json:"inlinedResponses"`
		} `json:"output"`
	} `json:"response"`
	Dest struct {
		InlinedResponses inlinedList `json:"inlinedResponses"`
	} `json:"dest"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *batchPollResponse) state() string {
	for _, s := range []string{p.Metadata.State, p.Response.State, p.State} {
		if s != "" {
			return s
		}
	}
	return ""
}

func (p *batchPollResponse) inlined() []batchInlined {
	if len(p.Response.InlinedResponses) > 0 {
		return p.Response.InlinedResponses
	}
	if len(p.Response.Output.InlinedResponses) > 0 {
		return p.Response.Output.InlinedResponses
	}
	return p.Dest.InlinedResponses
}

// normalizeBatchState maps a wire state ("JOB_STATE_SUCCEEDED",
// "BATCH_STATE_SUCCEEDED", "SUCCEEDED", ...) onto the BatchState* constants by
// suffix, so either documented prefix (or none) parses.
func normalizeBatchState(wire string) string {
	u := strings.ToUpper(strings.TrimSpace(wire))
	for _, s := range []string{
		BatchStateSucceeded, BatchStateFailed, BatchStateCancelled,
		BatchStateExpired, BatchStateRunning, BatchStatePending,
	} {
		if strings.HasSuffix(u, s) {
			return s
		}
	}
	return BatchStateUnknown
}

// GetScoreBatch polls a submitted batch by name. It is a single-shot read (no
// retries): the orchestrator re-polls every tick anyway, so a transient error
// just means "try again next tick".
func (c *BatchClient) GetScoreBatch(ctx context.Context, name string) (BatchStatus, error) {
	start := time.Now()
	url := c.root + "/" + strings.TrimPrefix(name, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return BatchStatus{}, err
	}
	req.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		metrics.ObserveGeminiRequest("score_batch_poll", "error", "transport", "overflow", time.Since(start))
		return BatchStatus{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		metrics.ObserveGeminiRequest("score_batch_poll", "error", statusLabel(resp.StatusCode), "overflow", time.Since(start))
		return BatchStatus{}, err
	}

	var parsed batchPollResponse
	if err := json.Unmarshal(raw, &parsed); err != nil && resp.StatusCode == http.StatusOK {
		metrics.ObserveGeminiRequest("score_batch_poll", "error", statusLabel(resp.StatusCode), "overflow", time.Since(start))
		return BatchStatus{}, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := parsed.Error.Message
		if msg == "" {
			msg = fmt.Sprintf("gemini batch error %d", resp.StatusCode)
		}
		metrics.ObserveGeminiRequest("score_batch_poll", "error", statusLabel(resp.StatusCode), "overflow", time.Since(start))
		return BatchStatus{}, errors.New(msg)
	}
	metrics.ObserveGeminiRequest("score_batch_poll", "ok", statusLabel(resp.StatusCode), "overflow", time.Since(start))

	st := BatchStatus{
		Name:  parsed.Name,
		State: normalizeBatchState(parsed.state()),
		Err:   parsed.Error.Message,
	}
	if st.Name == "" {
		st.Name = name
	}
	for _, in := range parsed.inlined() {
		r := BatchResult{Key: in.Metadata.Key}
		if in.Error != nil && in.Error.Message != "" {
			r.Err = in.Error.Message
		} else if in.Response != nil {
			if in.Response.Error.Message != "" {
				r.Err = in.Response.Error.Message
			} else if len(in.Response.Candidates) > 0 && len(in.Response.Candidates[0].Content.Parts) > 0 {
				r.Text = strings.TrimSpace(in.Response.Candidates[0].Content.Parts[0].Text)
			}
			u := in.Response.UsageMetadata
			r.PromptTokens = u.PromptTokenCount
			r.CachedTokens = u.CachedContentTokenCount
			r.CandidateTokens = u.CandidatesTokenCount
			r.ThoughtTokens = u.ThoughtsTokenCount
			r.TotalTokens = u.TotalTokenCount
		}
		st.Results = append(st.Results, r)
	}
	return st, nil
}
