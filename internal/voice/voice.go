// Package voice ports lib/voice-check.js: it flags AI-flavored / off-voice text
// before it goes into an application answer. Local heuristics (kill-list words,
// dash connectors, banned openers, low sentence-length burstiness) run offline;
// Sapling and HuggingFace AI-detection are optional and only run when their API
// keys are set. Backs the /app-questions and /apply voice-check gate.
package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"unicode"
)

var killList = []string{
	"delve", "dive into", "seamless", "robust", "tapestry", "testament",
	"synergy", "elevate", "multifaceted", "pivotal", "realm", "cutting-edge",
	"spearheaded", "furthermore", "moreover", "additionally", "cutting edge",
	"not only", "it's important to note", "in today's", "proven track record",
	"i'm drawn to", "i am drawn to", "i appreciate the opportunity",
	"passionate about", "excited to", "strong passion", "deeply passionate",
}

var bannedOpeners = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^my background is`),
	regexp.MustCompile(`(?i)^i('ve| have) spent (my career|the last|years)`),
	regexp.MustCompile(`(?i)^i have (extensive|significant|deep|strong) experience`),
	regexp.MustCompile(`(?i)^throughout my career`),
	regexp.MustCompile(`(?i)^i('m| am) (also )?(drawn to|excited by|passionate about|enthusiastic about)`),
	regexp.MustCompile(`(?i)^i find (this|that|it)`),
	regexp.MustCompile(`(?i)^as (a|an) (senior|experienced|seasoned)`),
	regexp.MustCompile(`(?i)^in my experience`),
	regexp.MustCompile(`(?i)^i have a proven`),
	regexp.MustCompile(`(?i)^with (my|over|more than) \d`),
}

var dashPattern = regexp.MustCompile(`\s[—–-]{1,2}\s`)

// Thresholds for the external detectors and the local pass/fail.
const (
	saplingThreshold = 0.02
	hfThreshold      = 0.3
)

// Issue is one local heuristic flag. Type is one of kill_word, dash,
// banned_opener, low_burstiness.
type Issue struct {
	Type   string
	Detail string
}

// DetectorResult is one external detector's outcome. Nil means "no API key".
type DetectorResult struct {
	Score          float64
	SentenceScores []SentenceScore
	Err            string
}

// SentenceScore is one Sapling per-sentence AI score.
type SentenceScore struct {
	Sentence string  `json:"sentence"`
	Score    float64 `json:"score"`
}

// Result is the full voice check.
type Result struct {
	Text        string
	Issues      []Issue
	Sapling     *DetectorResult
	HuggingFace *DetectorResult
	Passed      bool
}

// splitSentences reproduces text.split(/(?<=[.!?])\s+/): split on a whitespace
// run whose preceding char is sentence-ending punctuation, keeping the
// punctuation on the left segment. Go's RE2 has no lookbehind, so do it by hand.
func splitSentences(text string) []string {
	runes := []rune(text)
	var out []string
	start := 0
	for i := 0; i < len(runes); {
		if unicode.IsSpace(runes[i]) && i > 0 && isSentenceEnd(runes[i-1]) {
			out = append(out, string(runes[start:i]))
			j := i
			for j < len(runes) && unicode.IsSpace(runes[j]) {
				j++
			}
			start = j
			i = j
			continue
		}
		i++
	}
	out = append(out, string(runes[start:]))
	return out
}

func isSentenceEnd(r rune) bool { return r == '.' || r == '!' || r == '?' }

func wordCount(s string) int { return len(strings.Fields(s)) }

// LocalCheck runs the offline heuristics. Exported for testing.
func LocalCheck(text string) []Issue {
	var issues []Issue
	lower := strings.ToLower(text)

	for _, word := range killList {
		if strings.Contains(lower, word) {
			issues = append(issues, Issue{Type: "kill_word", Detail: `"` + word + `"`})
		}
	}

	if m := dashPattern.FindAllString(text, -1); len(m) > 0 {
		issues = append(issues, Issue{Type: "dash", Detail: fmt.Sprintf("%d dash connector(s) found", len(m))})
	}

	sentences := splitSentences(text)
	for _, sentence := range sentences {
		trimmed := strings.TrimSpace(sentence)
		for _, pattern := range bannedOpeners {
			if pattern.MatchString(trimmed) {
				snippet := trimmed
				if len([]rune(snippet)) > 60 {
					snippet = string([]rune(snippet)[:60])
				}
				issues = append(issues, Issue{Type: "banned_opener", Detail: `"` + snippet + `..."`})
				break
			}
		}
	}

	var lengths []int
	for _, s := range sentences {
		if n := wordCount(strings.TrimSpace(s)); n > 2 {
			lengths = append(lengths, n)
		}
	}
	if len(lengths) >= 3 {
		var sum float64
		for _, n := range lengths {
			sum += float64(n)
		}
		mean := sum / float64(len(lengths))
		var variance float64
		for _, n := range lengths {
			variance += math.Pow(float64(n)-mean, 2)
		}
		variance /= float64(len(lengths))
		stddev := math.Sqrt(variance)
		if stddev < 4 {
			issues = append(issues, Issue{
				Type:   "low_burstiness",
				Detail: fmt.Sprintf("sentence length std dev is %.1f (want > 4, higher = more varied)", stddev),
			})
		}
	}

	return issues
}

func saplingCheck(ctx context.Context, client *http.Client, text, apiKey string) *DetectorResult {
	if apiKey == "" {
		return nil
	}
	payload, _ := json.Marshal(map[string]string{"key": apiKey, "text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.sapling.ai/api/v1/aidetect", bytes.NewReader(payload))
	if err != nil {
		return &DetectorResult{Err: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return &DetectorResult{Err: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &DetectorResult{Err: fmt.Sprintf("Sapling API error %d: %s", resp.StatusCode, truncate(string(body), 100))}
	}
	var data struct {
		Score          float64         `json:"score"`
		SentenceScores []SentenceScore `json:"sentence_scores"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return &DetectorResult{Err: err.Error()}
	}
	return &DetectorResult{Score: data.Score, SentenceScores: data.SentenceScores}
}

func huggingFaceCheck(ctx context.Context, client *http.Client, text, apiKey string) *DetectorResult {
	if apiKey == "" {
		return nil
	}
	payload, _ := json.Marshal(map[string]string{"inputs": text})
	const url = "https://router.huggingface.co/hf-inference/models/openai-community/roberta-base-openai-detector"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return &DetectorResult{Err: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return &DetectorResult{Err: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &DetectorResult{Err: fmt.Sprintf("HuggingFace API error %d: %s", resp.StatusCode, truncate(string(body), 100))}
	}
	// Response is double-nested: [[{label, score}, ...]] (or single-nested).
	type entry struct {
		Label string  `json:"label"`
		Score float64 `json:"score"`
	}
	var nested [][]entry
	var entries []entry
	if err := json.Unmarshal(body, &nested); err == nil && len(nested) > 0 {
		entries = nested[0]
	} else if err := json.Unmarshal(body, &entries); err != nil {
		return &DetectorResult{Err: err.Error()}
	}
	for _, want := range []string{"Fake", "ChatGPT"} {
		for _, e := range entries {
			if e.Label == want {
				return &DetectorResult{Score: e.Score}
			}
		}
	}
	return &DetectorResult{Err: "Unexpected response format from HuggingFace"}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// RenderScore formats an AI-detection score the way check-voice.js did.
func RenderScore(score float64) string {
	switch {
	case score < 0.02:
		return fmt.Sprintf("%.1f%% AI  (looks human)", score*100)
	case score < 0.3:
		return fmt.Sprintf("%.0f%% AI  (borderline)", score*100)
	default:
		return fmt.Sprintf("%.0f%% AI  (flagging — rewrite)", score*100)
	}
}

// CheckVoiceText runs the local heuristics plus (when keyed) the external
// detectors, and computes the overall pass/fail. client is optional.
func CheckVoiceText(ctx context.Context, client *http.Client, text, saplingKey, hfKey string) Result {
	if client == nil {
		client = http.DefaultClient
	}
	issues := LocalCheck(text)

	var sapling, hf *DetectorResult
	done := make(chan struct{}, 2)
	go func() { sapling = saplingCheck(ctx, client, text, saplingKey); done <- struct{}{} }()
	go func() { hf = huggingFaceCheck(ctx, client, text, hfKey); done <- struct{}{} }()
	<-done
	<-done

	localFailed := false
	for _, i := range issues {
		if i.Type != "low_burstiness" {
			localFailed = true
			break
		}
	}
	saplingFailed := sapling != nil && sapling.Err == "" && sapling.Score >= saplingThreshold
	hfFailed := hf != nil && hf.Err == "" && hf.Score >= hfThreshold

	return Result{
		Text:        text,
		Issues:      issues,
		Sapling:     sapling,
		HuggingFace: hf,
		Passed:      !localFailed && !saplingFailed && !hfFailed,
	}
}
