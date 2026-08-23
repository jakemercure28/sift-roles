package opsagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The agent makes exactly ONE model call per investigation. It is not an
// agentic tool loop, and that is a constraint rather than a simplification:
// the Gemini free tier allows 20 requests/day on gemini-3.6-flash, and a
// tool-using loop spends 10-15 calls on a single investigation. Evidence is
// therefore gathered deterministically (see evidence.go) and the model is
// asked once to interpret it.
//
// The upside is that the model is additive rather than load-bearing. If it is
// rate-limited, unavailable, or returns nonsense, the collected evidence is
// still filed as a GitHub issue. Alert delivery — the thing that was actually
// broken — never depends on an LLM being reachable.

const geminiEndpoint = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent"

// verdict is the model's structured answer. The schema is enforced server-side
// via responseSchema, so a malformed reply is a transport bug rather than
// something to defensively parse.
type verdict struct {
	Diagnosis string `json:"diagnosis"`
	Action    string `json:"action"`
	Target    string `json:"target,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Reason    string `json:"reason"`
	Command   string `json:"command,omitempty"`
}

// responseSchema pins the reply shape. `action` is an enum of the gate's
// vocabulary plus "escalate"/"none", so the model cannot invent a capability:
// anything it returns still has to survive checkAction before it runs.
var responseSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"diagnosis": map[string]any{
			"type":        "string",
			"description": "What is actually wrong, in two or three sentences, grounded in the evidence provided.",
		},
		"action": map[string]any{
			"type": "string",
			"enum": []string{
				ActionRolloutRestart, ActionDeletePod, ActionScaleDeploy, ActionArgoSync,
				"escalate", "none",
			},
			"description": "The single action to take. Use escalate when a human must act, none when nothing is wrong.",
		},
		"namespace": map[string]any{"type": "string", "description": "Namespace for the action. Empty for escalate/none/argocd_sync."},
		"target":    map[string]any{"type": "string", "description": "Deployment, pod, or Argo application the action applies to."},
		"reason":    map[string]any{"type": "string", "description": "Why this action and not another. Name the evidence you relied on."},
		"command":   map[string]any{"type": "string", "description": "For escalate: the exact command a human should run, if there is one."},
	},
	"required": []string{"diagnosis", "action", "reason"},
}

// geminiClient is a deliberately small single-shot client. It does not use
// internal/scorer's client: that one is prompt->text with no structured
// output, and importing it would drag the whole scoring package into what is
// otherwise a leaf.
type geminiClient struct {
	apiKey   string
	model    string
	fallback string
	http     *http.Client
	logger   *slog.Logger

	mu      sync.Mutex
	day     string
	spent   int
	budget  int
	tickets func() time.Time // injectable clock, for tests
}

func newGeminiClient(cfg Config, logger *slog.Logger) *geminiClient {
	return &geminiClient{
		apiKey:   cfg.GeminiAPIKey,
		model:    cfg.Model,
		fallback: cfg.FallbackModel,
		http:     &http.Client{Timeout: 90 * time.Second},
		logger:   logger,
		budget:   cfg.DailyCallBudget,
		tickets:  time.Now,
	}
}

// errBudgetExhausted means we deliberately did not call the API. It is not a
// failure: the caller still files the evidence, just without a diagnosis.
var errBudgetExhausted = fmt.Errorf("daily model-call budget exhausted")

// claim reserves one call against the daily budget. The counter is in-memory,
// so a pod restart resets it — that is intentional. The budget exists to keep
// us inside a free tier we do not want to think about, not to enforce a hard
// contract, and Google enforces the real limit anyway. Resetting low is the
// safe direction to be wrong in.
func (g *geminiClient) claim() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	// Pacific, matching how api_usage keys its days elsewhere in this repo, so
	// a human comparing the two sees the same day boundary.
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		loc = time.UTC
	}
	today := g.tickets().In(loc).Format("2006-01-02")
	if today != g.day {
		g.day, g.spent = today, 0
	}
	if g.spent >= g.budget {
		return false
	}
	g.spent++
	return true
}

// spentToday reports budget consumption for logging and metrics.
func (g *geminiClient) spentToday() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.spent, g.budget
}

type geminiReq struct {
	Contents          []geminiContent  `json:"contents"`
	SystemInstruction *geminiContent   `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenConfig `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenConfig struct {
	ResponseMIMEType string         `json:"responseMimeType,omitempty"`
	ResponseSchema   map[string]any `json:"responseSchema,omitempty"`
	Temperature      *float64       `json:"temperature,omitempty"`
	MaxOutputTokens  int            `json:"maxOutputTokens,omitempty"`
}

type geminiResp struct {
	Candidates []struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// diagnose makes the single model call. On a 429 for the primary model it
// retries once on the fallback, which sits on a separate per-model quota
// (gemini-3.5-flash-lite has 500 rpd against gemini-3.6-flash's 20).
func (g *geminiClient) diagnose(ctx context.Context, system, evidence string) (verdict, int, error) {
	if !g.claim() {
		return verdict{}, 0, errBudgetExhausted
	}

	models := []string{g.model}
	if g.fallback != "" && g.fallback != g.model {
		models = append(models, g.fallback)
	}

	var lastErr error
	for _, model := range models {
		v, tokens, err := g.callOnce(ctx, model, system, evidence)
		if err == nil {
			return v, tokens, nil
		}
		lastErr = err
		if !isQuotaError(err) {
			break // a real error; a different model will not help
		}
		g.logger.Warn("opsagent: model quota exhausted, trying fallback",
			"model", model, "error", err)
	}
	return verdict{}, 0, lastErr
}

func (g *geminiClient) callOnce(ctx context.Context, model, system, evidence string) (verdict, int, error) {
	temp := 0.0
	body, err := json.Marshal(geminiReq{
		Contents:          []geminiContent{{Role: "user", Parts: []geminiPart{{Text: evidence}}}},
		SystemInstruction: &geminiContent{Parts: []geminiPart{{Text: system}}},
		GenerationConfig: &geminiGenConfig{
			ResponseMIMEType: "application/json",
			ResponseSchema:   responseSchema,
			Temperature:      &temp,
			MaxOutputTokens:  2048,
		},
	})
	if err != nil {
		return verdict{}, 0, fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf(geminiEndpoint, model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return verdict{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Header rather than ?key= so the credential stays out of any URL that
	// might be logged by an intermediary.
	req.Header.Set("x-goog-api-key", g.apiKey)

	resp, err := g.http.Do(req)
	if err != nil {
		return verdict{}, 0, fmt.Errorf("gemini request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return verdict{}, 0, fmt.Errorf("read response: %w", err)
	}

	var parsed geminiResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return verdict{}, 0, fmt.Errorf("decode response (status %d): %w", resp.StatusCode, err)
	}
	if parsed.Error != nil {
		return verdict{}, 0, &geminiAPIError{Code: parsed.Error.Code, Message: parsed.Error.Message}
	}
	if resp.StatusCode != http.StatusOK {
		return verdict{}, 0, &geminiAPIError{Code: resp.StatusCode, Message: string(raw)}
	}
	if len(parsed.Candidates) == 0 || len(parsed.Candidates[0].Content.Parts) == 0 {
		return verdict{}, 0, fmt.Errorf("gemini returned no candidates")
	}

	var text strings.Builder
	for _, p := range parsed.Candidates[0].Content.Parts {
		text.WriteString(p.Text)
	}

	var v verdict
	if err := json.Unmarshal([]byte(text.String()), &v); err != nil {
		return verdict{}, parsed.UsageMetadata.TotalTokenCount,
			fmt.Errorf("decode verdict: %w", err)
	}
	return v, parsed.UsageMetadata.TotalTokenCount, nil
}

type geminiAPIError struct {
	Code    int
	Message string
}

func (e *geminiAPIError) Error() string {
	return fmt.Sprintf("gemini API error %d: %s", e.Code, truncate(e.Message, 200))
}

func isQuotaError(err error) bool {
	var apiErr *geminiAPIError
	if ok := asGeminiError(err, &apiErr); ok {
		return apiErr.Code == http.StatusTooManyRequests
	}
	return false
}

func asGeminiError(err error, target **geminiAPIError) bool {
	for err != nil {
		if e, ok := err.(*geminiAPIError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
