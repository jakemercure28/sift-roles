package scorer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseScoreResponse(t *testing.T) {
	tests := []struct {
		name         string
		text         string
		wantScore    *int
		wantContains string
	}{
		{
			name:         "well formed",
			text:         "SCORE: 8\nREASONING: Strong DevOps match across the stack.",
			wantScore:    intPtr(8),
			wantContains: "Strong DevOps match",
		},
		{
			name:      "clamps above ten",
			text:      "SCORE: 42\nREASONING: x",
			wantScore: intPtr(10),
		},
		{
			name:      "clamps below one",
			text:      "SCORE: 0\nREASONING: x",
			wantScore: intPtr(1),
		},
		{
			name:         "score only, reasoning missing -> full text",
			text:         "SCORE: 5",
			wantScore:    intPtr(5),
			wantContains: "SCORE: 5",
		},
		{
			name:         "unparsable -> nil score + diagnostic",
			text:         "the model rambled without a score",
			wantScore:    nil,
			wantContains: "Score parse failed",
		},
		{
			name:         "multiline reasoning captured",
			text:         "SCORE: 7\nREASONING: line one\nline two\nline three",
			wantScore:    intPtr(7),
			wantContains: "line three",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseScoreResponse(tt.text)
			if !eqIntPtr(got.Score, tt.wantScore) {
				t.Fatalf("score = %v, want %v", ptrStr(got.Score), ptrStr(tt.wantScore))
			}
			if tt.wantContains != "" && !strings.Contains(got.Reasoning, tt.wantContains) {
				t.Fatalf("reasoning %q does not contain %q", got.Reasoning, tt.wantContains)
			}
		})
	}
}

func TestSharedLimiterSpacesCalls(t *testing.T) {
	l := &sharedLimiter{delay: 40 * time.Millisecond}
	ctx := context.Background()

	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := l.acquire(ctx, "t"); err != nil {
			t.Fatalf("acquire: %v", err)
		}
	}
	// First call is immediate, next two are spaced one delay apart: >= 2*delay.
	if elapsed := time.Since(start); elapsed < 70*time.Millisecond {
		t.Fatalf("3 calls took %v, expected >= ~80ms of spacing", elapsed)
	}
}

func TestSharedLimiterHonorsContext(t *testing.T) {
	l := &sharedLimiter{delay: time.Hour}
	_ = l.acquire(context.Background(), "t") // first call reserves now, returns immediately

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.acquire(ctx, "t"); err == nil {
		t.Fatal("expected context error while waiting for a far-future slot")
	}
}

// TestLimiterForSharesByKey proves clients built with the same key share one
// limiter (process-global pacing) while different keys do not pace each other.
func TestLimiterForSharesByKey(t *testing.T) {
	a1 := limiterFor("key-A", Model, time.Second)
	a2 := limiterFor("key-A", Model, time.Second)
	b := limiterFor("key-B", Model, time.Second)
	if a1 != a2 {
		t.Fatal("same key+model returned different limiters")
	}
	if a1 == b {
		t.Fatal("different keys returned the same limiter")
	}
	// Same key, different model must not share a limiter: Gemini RPM is per-model,
	// so models on one key must pace independently.
	if m := limiterFor("key-A", "gemini-other-model", time.Second); m == a1 {
		t.Fatal("same key but different model returned the same limiter")
	}
	if got := limiterFor("", Model, time.Second); got.delay != 0 {
		t.Fatalf("empty key limiter delay = %v, want 0 (no-op)", got.delay)
	}
}

// TestSharedLimiterIsFairAcrossTenants proves one tenant flooding the queue does
// not starve another: a small tenant's calls interleave rather than landing behind
// the whole backlog. Tenant A enqueues many, tenant B two; B should finish well
// before A's last grant.
func TestSharedLimiterIsFairAcrossTenants(t *testing.T) {
	l := &sharedLimiter{delay: 10 * time.Millisecond}
	ctx := context.Background()

	var wg sync.WaitGroup
	grantOrder := make([]string, 0, 12)
	var mu sync.Mutex

	// Enqueue A's flood first so its waiters are all registered ahead of B's.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.acquire(ctx, "A"); err != nil {
				t.Errorf("A acquire: %v", err)
				return
			}
			mu.Lock()
			grantOrder = append(grantOrder, "A")
			mu.Unlock()
		}()
	}
	// Give A's goroutines a head start to register before B arrives.
	time.Sleep(5 * time.Millisecond)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := l.acquire(ctx, "B"); err != nil {
				t.Errorf("B acquire: %v", err)
				return
			}
			mu.Lock()
			grantOrder = append(grantOrder, "B")
			mu.Unlock()
		}()
	}
	wg.Wait()

	// Find the position of B's last grant. With round-robin it should land within
	// the first few grants, not after all 10 of A's (which FIFO would produce).
	lastB := -1
	for i, who := range grantOrder {
		if who == "B" {
			lastB = i
		}
	}
	if lastB < 0 {
		t.Fatal("B was never granted")
	}
	if lastB > 5 {
		t.Fatalf("B's last grant at position %d (order %v); round-robin should interleave it early", lastB, grantOrder)
	}
}

// TestSharedLimiterDropsCancelledWaiter proves a cancelled waiter does not consume
// a slot: a later live waiter is still served promptly.
func TestSharedLimiterDropsCancelledWaiter(t *testing.T) {
	l := &sharedLimiter{delay: 30 * time.Millisecond}

	// First acquire takes the immediate slot.
	if err := l.acquire(context.Background(), "x"); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// This one reserves the next (future) slot but cancels before it is granted.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := l.acquire(ctx, "x"); err == nil {
		t.Fatal("expected cancellation error")
	}

	// A live waiter should still be served around one delay later, not two.
	start := time.Now()
	if err := l.acquire(context.Background(), "x"); err != nil {
		t.Fatalf("third acquire: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 60*time.Millisecond {
		t.Fatalf("live waiter took %v; cancelled waiter should not have consumed extra slots", elapsed)
	}
}

func TestCallGeminiSuccessRecordsUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-goog-api-key"); got != "test-key" {
			t.Errorf("api key header = %q", got)
		}
		writeCandidate(w, "SCORE: 9\nREASONING: ok")
	}))
	defer srv.Close()

	usage := &fakeUsage{}
	c := newTestClient(srv.URL, "test-key", usage)

	text, err := c.CallGemini(context.Background(), "prompt", 600)
	if err != nil {
		t.Fatalf("CallGemini: %v", err)
	}
	if text != "SCORE: 9\nREASONING: ok" {
		t.Fatalf("unexpected text: %q", text)
	}
	if usage.calls.Load() != 1 {
		t.Fatalf("usage recorded %d times, want 1", usage.calls.Load())
	}
}

func TestCallGeminiSuccessRecordsTokenUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCandidateWithUsage(w, "SCORE: 8\nREASONING: ok", geminiUsageMetadata{
			PromptTokenCount:        1200,
			CachedContentTokenCount: 800,
			CandidatesTokenCount:    80,
			TotalTokenCount:         1280,
		})
	}))
	defer srv.Close()

	usage := &fakeUsage{}
	c := newTestClient(srv.URL, "test-key", usage).WithTenant("tenant-a").WithHostKey(true)

	if _, err := c.CallGeminiScore(context.Background(), "prompt", 600); err != nil {
		t.Fatalf("CallGeminiScore: %v", err)
	}
	if usage.calls.Load() != 1 || usage.hostCalls.Load() != 1 {
		t.Fatalf("calls=%d hostCalls=%d, want 1/1", usage.calls.Load(), usage.hostCalls.Load())
	}
	if usage.promptTokens.Load() != 1200 || usage.cachedPromptTokens.Load() != 800 ||
		usage.candidateTokens.Load() != 80 || usage.totalTokens.Load() != 1280 {
		t.Fatalf("tokens prompt=%d cached=%d candidates=%d total=%d",
			usage.promptTokens.Load(),
			usage.cachedPromptTokens.Load(),
			usage.candidateTokens.Load(),
			usage.totalTokens.Load(),
		)
	}
	if usage.hostPromptTokens.Load() != 1200 || usage.hostCachedPromptTokens.Load() != 800 ||
		usage.hostCandidateTokens.Load() != 80 || usage.hostTotalTokens.Load() != 1280 {
		t.Fatalf("host tokens prompt=%d cached=%d candidates=%d total=%d",
			usage.hostPromptTokens.Load(),
			usage.hostCachedPromptTokens.Load(),
			usage.hostCandidateTokens.Load(),
			usage.hostTotalTokens.Load(),
		)
	}
}

func TestCallGeminiRetriesOn429ThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		writeCandidate(w, "SCORE: 6\nREASONING: recovered")
	}))
	defer srv.Close()

	usage := &fakeUsage{}
	c := newTestClient(srv.URL, "test-key", usage)
	c.retry429 = 1 * time.Millisecond // don't wait 30s in the test

	text, err := c.CallGemini(context.Background(), "prompt", 600)
	if err != nil {
		t.Fatalf("CallGemini: %v", err)
	}
	if !strings.Contains(text, "recovered") {
		t.Fatalf("unexpected text: %q", text)
	}
	if hits.Load() != 2 {
		t.Fatalf("server hit %d times, want 2 (one 429 + one success)", hits.Load())
	}
	if usage.calls.Load() != 1 {
		t.Fatalf("usage recorded %d, want 1 (only the success)", usage.calls.Load())
	}
}

func TestCallGeminiNoAPIKey(t *testing.T) {
	c := newTestClient("http://unused", "", &fakeUsage{})
	if _, err := c.CallGemini(context.Background(), "p", 600); err == nil {
		t.Fatal("expected error when API key is empty")
	}
}

// --- helpers ---

type fakeUsage struct {
	calls                  atomic.Int32
	hostCalls              atomic.Int32
	promptTokens           atomic.Int32
	cachedPromptTokens     atomic.Int32
	candidateTokens        atomic.Int32
	totalTokens            atomic.Int32
	hostPromptTokens       atomic.Int32
	hostCachedPromptTokens atomic.Int32
	hostCandidateTokens    atomic.Int32
	hostTotalTokens        atomic.Int32
}

func (f *fakeUsage) RecordAPICall(string) error     { f.calls.Add(1); return nil }
func (f *fakeUsage) RecordHostAPICall(string) error { f.hostCalls.Add(1); return nil }
func (f *fakeUsage) RecordAPITokens(_ string, promptTokens, cachedPromptTokens, candidateTokens, totalTokens int) error {
	f.promptTokens.Add(int32(promptTokens))
	f.cachedPromptTokens.Add(int32(cachedPromptTokens))
	f.candidateTokens.Add(int32(candidateTokens))
	f.totalTokens.Add(int32(totalTokens))
	return nil
}
func (f *fakeUsage) RecordHostAPITokens(_ string, promptTokens, cachedPromptTokens, candidateTokens, totalTokens int) error {
	f.hostPromptTokens.Add(int32(promptTokens))
	f.hostCachedPromptTokens.Add(int32(cachedPromptTokens))
	f.hostCandidateTokens.Add(int32(candidateTokens))
	f.hostTotalTokens.Add(int32(totalTokens))
	return nil
}

func newTestClient(baseURL, key string, usage UsageRecorder) *Client {
	c := NewClient(key, 0, usage, nil)
	c.baseURL = baseURL
	c.retryBase = time.Millisecond
	return c
}

func writeCandidate(w http.ResponseWriter, text string) {
	writeCandidateWithUsage(w, text, geminiUsageMetadata{})
}

func writeCandidateWithUsage(w http.ResponseWriter, text string, usage geminiUsageMetadata) {
	resp := geminiResponse{}
	resp.Candidates = append(resp.Candidates, struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
	}{})
	resp.Candidates[0].Content.Parts = []geminiPart{{Text: text}}
	resp.UsageMetadata = usage
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func intPtr(n int) *int { return &n }

func eqIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func ptrStr(p *int) string {
	if p == nil {
		return "nil"
	}
	return strconv.Itoa(*p)
}
