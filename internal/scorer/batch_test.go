package scorer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestBatchClient(root string) *BatchClient {
	c := NewBatchClient("overflow-key", nil)
	c.root = root
	c.retryBase = time.Millisecond
	return c
}

// Cancel must POST the documented :cancel verb with the key header, and a
// non-200 must surface the server's error message (single-shot, no retries:
// the caller treats failure as best-effort).
func TestCancelScoreBatch(t *testing.T) {
	var gotPath, gotMethod, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotKey = r.Header.Get("x-goog-api-key")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if err := newTestBatchClient(srv.URL).CancelScoreBatch(context.Background(), "batches/abc"); err != nil {
		t.Fatalf("CancelScoreBatch: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/batches/abc:cancel" || gotKey != "overflow-key" {
		t.Fatalf("cancel request = %s %s key=%q, want POST /batches/abc:cancel with the key", gotMethod, gotPath, gotKey)
	}

	var calls atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"message":"batch not found"}}`))
	}))
	defer bad.Close()
	err := newTestBatchClient(bad.URL).CancelScoreBatch(context.Background(), "batches/gone")
	if err == nil || !strings.Contains(err.Error(), "batch not found") {
		t.Fatalf("cancel error = %v, want the server message", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("cancel calls = %d, want exactly 1 (no retries)", calls.Load())
	}
}

// Submit must place each item's prompt and correlation key in the documented
// inline-request envelope, carry the sync path's exact generationConfig, and
// return the server-assigned batch name.
func TestSubmitScoreBatch(t *testing.T) {
	var gotPath, gotKey string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		gotBody, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"name":"batches/abc123"}`))
	}))
	defer srv.Close()

	c := newTestBatchClient(srv.URL)
	name, err := c.SubmitScoreBatch(context.Background(), "test-batch", []BatchItem{
		{Key: "job-1", Prompt: "prompt one"},
		{Key: "job-2", Prompt: "prompt two"},
	})
	if err != nil {
		t.Fatalf("SubmitScoreBatch: %v", err)
	}
	if name != "batches/abc123" {
		t.Fatalf("name = %q, want batches/abc123", name)
	}
	if gotPath != "/models/"+Model+":batchGenerateContent" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotKey != "overflow-key" {
		t.Fatalf("api key header = %q", gotKey)
	}

	var body struct {
		Batch struct {
			DisplayName string `json:"displayName"`
			InputConfig struct {
				Requests struct {
					Requests []struct {
						Request struct {
							Contents []struct {
								Parts []struct {
									Text string `json:"text"`
								} `json:"parts"`
							} `json:"contents"`
							GenerationConfig struct {
								MaxOutputTokens int      `json:"maxOutputTokens"`
								Temperature     *float64 `json:"temperature"`
								ThinkingConfig  *struct {
									ThinkingLevel string `json:"thinkingLevel"`
								} `json:"thinkingConfig"`
							} `json:"generationConfig"`
						} `json:"request"`
						Metadata map[string]string `json:"metadata"`
					} `json:"requests"`
				} `json:"requests"`
			} `json:"inputConfig"`
		} `json:"batch"`
	}
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("decode submit body: %v", err)
	}
	reqs := body.Batch.InputConfig.Requests.Requests
	if body.Batch.DisplayName != "test-batch" || len(reqs) != 2 {
		t.Fatalf("displayName=%q reqs=%d", body.Batch.DisplayName, len(reqs))
	}
	if reqs[1].Metadata["key"] != "job-2" || reqs[1].Request.Contents[0].Parts[0].Text != "prompt two" {
		t.Fatalf("second item = %+v", reqs[1])
	}
	gc := reqs[0].Request.GenerationConfig
	if gc.MaxOutputTokens != scoreMaxOutputToken {
		t.Fatalf("maxOutputTokens = %d, want %d", gc.MaxOutputTokens, scoreMaxOutputToken)
	}
	if gc.Temperature == nil || *gc.Temperature != ScoreTemperature {
		t.Fatalf("temperature = %v, want %v", gc.Temperature, ScoreTemperature)
	}
	if gc.ThinkingConfig != nil {
		t.Fatal("thinkingConfig must be omitted when no level is set")
	}
}

// A 5xx on submit retries; a 429 surfaces ErrQuotaExhausted with no retries
// (the paid key's quota will not come back within a backoff window); a plain
// 400 fails immediately.
func TestSubmitScoreBatchRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":{"message":"backend blip"}}`))
			return
		}
		w.Write([]byte(`{"name":"batches/retry-ok"}`))
	}))
	defer srv.Close()

	c := newTestBatchClient(srv.URL)
	name, err := c.SubmitScoreBatch(context.Background(), "b", []BatchItem{{Key: "k", Prompt: "p"}})
	if err != nil || name != "batches/retry-ok" {
		t.Fatalf("retry submit = (%q, %v)", name, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}

	var quotaCalls atomic.Int32
	quota := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		quotaCalls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"quota exceeded"}}`))
	}))
	defer quota.Close()
	_, err = newTestBatchClient(quota.URL).SubmitScoreBatch(context.Background(), "b", []BatchItem{{Key: "k", Prompt: "p"}})
	if !errors.Is(err, ErrQuotaExhausted) || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("429 error = %v, want ErrQuotaExhausted wrapping the server message", err)
	}
	if quotaCalls.Load() != 1 {
		t.Fatalf("429 retried: calls = %d, want 1", quotaCalls.Load())
	}

	var badCalls atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer bad.Close()
	c2 := newTestBatchClient(bad.URL)
	if _, err := c2.SubmitScoreBatch(context.Background(), "b", []BatchItem{{Key: "k", Prompt: "p"}}); err == nil {
		t.Fatal("400 must fail")
	}
	if badCalls.Load() != 1 {
		t.Fatalf("400 retried: calls = %d, want 1", badCalls.Load())
	}
}

// Poll must normalize every documented state spelling and find the inlined
// responses in each documented location.
func TestGetScoreBatchShapes(t *testing.T) {
	item := `{"metadata":{"key":"job-9"},"response":{"candidates":[{"content":{"parts":[{"text":"Score: 7\nReasoning: solid match"}]}}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20,"thoughtsTokenCount":5,"totalTokenCount":125}}}`
	cases := []struct {
		name, body, wantState string
		wantResults           int
	}{
		{"guide flat running", `{"name":"batches/x","state":"JOB_STATE_RUNNING"}`, BatchStateRunning, 0},
		{"reference operation pending", `{"name":"batches/x","metadata":{"state":"BATCH_STATE_PENDING"}}`, BatchStatePending, 0},
		{"bare succeeded, dest", `{"name":"batches/x","state":"SUCCEEDED","dest":{"inlinedResponses":[` + item + `]}}`, BatchStateSucceeded, 1},
		{"operation succeeded, response flat", `{"name":"batches/x","done":true,"metadata":{"state":"BATCH_STATE_SUCCEEDED"},"response":{"inlinedResponses":[` + item + `]}}`, BatchStateSucceeded, 1},
		{"operation succeeded, nested output", `{"name":"batches/x","done":true,"response":{"state":"BATCH_STATE_SUCCEEDED","output":{"inlinedResponses":{"inlinedResponses":[` + item + `]}}}}`, BatchStateSucceeded, 1},
		// The shape the live API actually returned on 2026-07-10: a finished
		// Operation whose response is a GenerateContentBatchOutput with the
		// inlined items wrapped in an extra InlinedResponses message object.
		// The pre-fix parser failed to unmarshal this ("cannot unmarshal
		// object into Go struct field ... of type []scorer.batchInlined").
		{"live operation, wrapped response", `{"name":"batches/x","done":true,"metadata":{"@type":"type.googleapis.com/google.ai.generativelanguage.v1main.GenerateContentBatch","state":"BATCH_STATE_SUCCEEDED","batchStats":{"requestCount":"1","successfulRequestCount":"1"}},"response":{"@type":"type.googleapis.com/google.ai.generativelanguage.v1main.GenerateContentBatchOutput","inlinedResponses":{"inlinedResponses":[` + item + `]}}}`, BatchStateSucceeded, 1},
		{"expired", `{"name":"batches/x","metadata":{"state":"JOB_STATE_EXPIRED"}}`, BatchStateExpired, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/batches/x" {
					t.Errorf("poll path = %q", r.URL.Path)
				}
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			st, err := newTestBatchClient(srv.URL).GetScoreBatch(context.Background(), "batches/x")
			if err != nil {
				t.Fatalf("GetScoreBatch: %v", err)
			}
			if st.State != tc.wantState {
				t.Fatalf("state = %q, want %q", st.State, tc.wantState)
			}
			if len(st.Results) != tc.wantResults {
				t.Fatalf("results = %d, want %d", len(st.Results), tc.wantResults)
			}
			if tc.wantResults == 1 {
				r := st.Results[0]
				if r.Key != "job-9" || r.Text == "" || r.Err != "" {
					t.Fatalf("result = %+v", r)
				}
				if r.PromptTokens != 100 || r.CandidateTokens != 20 || r.ThoughtTokens != 5 || r.TotalTokens != 125 {
					t.Fatalf("tokens = %+v", r)
				}
			}
		})
	}
}

// A per-item error must surface on that item without failing the poll, and
// terminal-state detection must cover all four terminal states.
func TestGetScoreBatchItemErrorAndDone(t *testing.T) {
	body := `{"name":"batches/x","metadata":{"state":"BATCH_STATE_SUCCEEDED"},"response":{"inlinedResponses":[
		{"metadata":{"key":"job-ok"},"response":{"candidates":[{"content":{"parts":[{"text":"Score: 3"}]}}]}},
		{"metadata":{"key":"job-bad"},"error":{"code":13,"message":"internal error"}}
	]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer srv.Close()

	st, err := newTestBatchClient(srv.URL).GetScoreBatch(context.Background(), "batches/x")
	if err != nil {
		t.Fatalf("GetScoreBatch: %v", err)
	}
	if len(st.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(st.Results))
	}
	if st.Results[0].Key != "job-ok" || st.Results[0].Err != "" {
		t.Fatalf("ok item = %+v", st.Results[0])
	}
	if st.Results[1].Key != "job-bad" || st.Results[1].Err != "internal error" {
		t.Fatalf("bad item = %+v", st.Results[1])
	}

	for state, want := range map[string]bool{
		BatchStateSucceeded: true, BatchStateFailed: true, BatchStateCancelled: true,
		BatchStateExpired: true, BatchStateRunning: false, BatchStatePending: false,
		BatchStateUnknown: false,
	} {
		if BatchDone(state) != want {
			t.Fatalf("BatchDone(%s) = %v, want %v", state, !want, want)
		}
	}
}

// ScorePrompt must emit the sync prompt (rubric + resume + job) and
// ParseResult must run the sync parse + calibration.
func TestScorePromptAndParseResult(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "resume.md"), []byte("# Test Resume\nDevOps engineer, Kubernetes."), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(nil, dir)
	job := Job{Title: "Platform Engineer", Company: "TestCo", Description: "Run Kubernetes."}

	prompt, err := s.ScorePrompt(job)
	if err != nil {
		t.Fatalf("ScorePrompt: %v", err)
	}
	for _, frag := range []string{"# Test Resume", "Platform Engineer", "STEP 1"} {
		if !strings.Contains(prompt, frag) {
			t.Fatalf("prompt missing %q", frag)
		}
	}

	res := s.ParseResult(job, "SCORE: 7\nREASONING: direct function match")
	if res.Score == nil || *res.Score != 7 || res.Reasoning != "direct function match" {
		t.Fatalf("ParseResult = %+v", res)
	}
}
