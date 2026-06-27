package scorer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseBatchResponse(t *testing.T) {
	t.Run("well formed maps by index and clamps", func(t *testing.T) {
		text := `[{"index":0,"score":8,"reasoning":"a"},{"index":1,"score":42,"reasoning":"b"},{"index":2,"score":0,"reasoning":"c"}]`
		got := parseBatchResponse(text, 3)
		if len(got) != 3 {
			t.Fatalf("got %d results, want 3", len(got))
		}
		if got[0].Score == nil || *got[0].Score != 8 {
			t.Fatalf("index 0 score = %v, want 8", ptrStr(got[0].Score))
		}
		if got[1].Score == nil || *got[1].Score != 10 {
			t.Fatalf("index 1 score = %v, want 10 (clamped)", ptrStr(got[1].Score))
		}
		if got[2].Score == nil || *got[2].Score != 1 {
			t.Fatalf("index 2 score = %v, want 1 (clamped)", ptrStr(got[2].Score))
		}
	})

	t.Run("drops out-of-range and missing indices", func(t *testing.T) {
		text := `[{"index":0,"score":7,"reasoning":"a"},{"index":9,"score":7,"reasoning":"x"}]`
		got := parseBatchResponse(text, 3)
		if _, ok := got[0]; !ok {
			t.Fatal("index 0 missing")
		}
		if _, ok := got[9]; ok {
			t.Fatal("out-of-range index 9 should be dropped")
		}
		if _, ok := got[1]; ok {
			t.Fatal("index 1 was never returned; should be absent")
		}
	})

	t.Run("tolerates code fences", func(t *testing.T) {
		text := "```json\n[{\"index\":0,\"score\":6,\"reasoning\":\"a\"}]\n```"
		got := parseBatchResponse(text, 1)
		if got[0].Score == nil || *got[0].Score != 6 {
			t.Fatalf("fenced JSON not parsed: %v", ptrStr(got[0].Score))
		}
	})

	t.Run("unparsable returns empty", func(t *testing.T) {
		if got := parseBatchResponse("not json at all", 3); len(got) != 0 {
			t.Fatalf("got %d results, want 0", len(got))
		}
	})
}

func TestScoringPromptsKeepVariableJobsLast(t *testing.T) {
	single := buildScorePrompt("resume", Job{Title: "variable job"})
	if strings.Index(single, "STEP 1") > strings.Index(single, "## Job Listing") {
		t.Fatalf("single prompt puts rubric after the variable job listing")
	}
	if !strings.HasSuffix(single, "No description available.") {
		t.Fatalf("single prompt should end with the variable job block, got tail %q", single[len(single)-40:])
	}

	batch := buildBatchPrompt("resume", []Job{{Title: "a"}, {Title: "b"}})
	if strings.Index(batch, "STEP 1") > strings.Index(batch, "## Job Listings") {
		t.Fatalf("batch prompt puts rubric after variable job listings")
	}
	if !strings.Contains(batch, "Include every job exactly once.\n\n## Job Listings") {
		t.Fatalf("batch prompt should declare output contract before variable listings")
	}
}

// TestScoreJobsReconciles drives the full batch path against a fake Gemini: the
// batch call drops one index, which must be re-scored individually.
func TestScoreJobsReconciles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "resume.md"), []byte("resume"), 0o644); err != nil {
		t.Fatal(err)
	}

	var batchHits, singleHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req geminiRequest
		_ = json.Unmarshal(body, &req)
		if req.GenerationConfig.ResponseMimeType == "application/json" {
			batchHits.Add(1)
			// Return only index 0 and 2; drop index 1 to force reconciliation.
			writeCandidate(w, `[{"index":0,"score":8,"reasoning":"a"},{"index":2,"score":9,"reasoning":"c"}]`)
			return
		}
		singleHits.Add(1)
		writeCandidate(w, "SCORE: 5\nREASONING: reconciled")
	}))
	defer srv.Close()

	s := New(newTestClient(srv.URL, "k", &fakeUsage{}), dir)
	jobs := []Job{{Title: "j0"}, {Title: "j1"}, {Title: "j2"}}

	results, err := s.ScoreJobs(context.Background(), jobs)
	if err != nil {
		t.Fatalf("ScoreJobs: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	if results[0].Score == nil || *results[0].Score != 8 {
		t.Fatalf("index 0 = %v, want 8", ptrStr(results[0].Score))
	}
	if results[1].Score == nil || *results[1].Score != 5 {
		t.Fatalf("index 1 = %v, want 5 (reconciled)", ptrStr(results[1].Score))
	}
	if results[2].Score == nil || *results[2].Score != 9 {
		t.Fatalf("index 2 = %v, want 9", ptrStr(results[2].Score))
	}
	if batchHits.Load() != 1 {
		t.Fatalf("batch calls = %d, want 1", batchHits.Load())
	}
	if singleHits.Load() != 1 {
		t.Fatalf("reconciliation calls = %d, want 1", singleHits.Load())
	}
}

// TestScoringPinsTemperature confirms both scoring paths send temperature 0 (for
// reproducible scores) while the shared CallGemini leaves it unset so discovery
// and ATS resolution keep the model default.
func TestScoringPinsTemperature(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "resume.md"), []byte("resume"), 0o644); err != nil {
		t.Fatal(err)
	}

	var lastTemp *float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req geminiRequest
		_ = json.Unmarshal(body, &req)
		lastTemp = req.GenerationConfig.Temperature
		if req.GenerationConfig.ResponseMimeType == "application/json" {
			writeCandidate(w, `[{"index":0,"score":3,"reasoning":"a"},{"index":1,"score":3,"reasoning":"b"}]`)
			return
		}
		writeCandidate(w, "SCORE: 3\nREASONING: ok")
	}))
	defer srv.Close()

	client := newTestClient(srv.URL, "k", &fakeUsage{})
	s := New(client, dir)

	// Single-job scoring path.
	if _, err := s.ScoreJob(context.Background(), Job{Title: "j"}); err != nil {
		t.Fatal(err)
	}
	if lastTemp == nil || *lastTemp != 0 {
		t.Fatalf("single-job temperature = %v, want 0", ptrStr2(lastTemp))
	}

	// Batch scoring path.
	if _, err := s.ScoreJobs(context.Background(), []Job{{Title: "a"}, {Title: "b"}}); err != nil {
		t.Fatal(err)
	}
	if lastTemp == nil || *lastTemp != 0 {
		t.Fatalf("batch temperature = %v, want 0", ptrStr2(lastTemp))
	}

	// Shared CallGemini (discovery/ATS) must leave temperature unset.
	if _, err := client.CallGemini(context.Background(), "hi", 64); err != nil {
		t.Fatal(err)
	}
	if lastTemp != nil {
		t.Fatalf("shared CallGemini temperature = %v, want unset", *lastTemp)
	}
}

func ptrStr2(p *float64) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprintf("%.1f", *p)
}

// TestScoreJobsTwoStage verifies that with a rescore threshold set, batch results
// at or above it are re-scored individually (stage two) and the single-job score
// replaces the batch score, while lower batch scores are left untouched.
func TestScoreJobsTwoStage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "resume.md"), []byte("resume"), 0o644); err != nil {
		t.Fatal(err)
	}

	var batchHits, singleHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req geminiRequest
		_ = json.Unmarshal(body, &req)
		if req.GenerationConfig.ResponseMimeType == "application/json" {
			batchHits.Add(1)
			// index 0 scores high (>= threshold) so it must be re-scored; index 1
			// scores low and must be left as-is.
			writeCandidate(w, `[{"index":0,"score":9,"reasoning":"batch high"},{"index":1,"score":4,"reasoning":"batch low"}]`)
			return
		}
		singleHits.Add(1)
		writeCandidate(w, "SCORE: 5\nREASONING: rescored on its own")
	}))
	defer srv.Close()

	s := New(newTestClient(srv.URL, "k", &fakeUsage{}), dir).WithRescoreThreshold(7)
	jobs := []Job{{Title: "j0"}, {Title: "j1"}}

	results, err := s.ScoreJobs(context.Background(), jobs)
	if err != nil {
		t.Fatalf("ScoreJobs: %v", err)
	}
	if results[0].Score == nil || *results[0].Score != 5 {
		t.Fatalf("index 0 = %v, want 5 (re-scored)", ptrStr(results[0].Score))
	}
	if results[1].Score == nil || *results[1].Score != 4 {
		t.Fatalf("index 1 = %v, want 4 (below threshold, untouched)", ptrStr(results[1].Score))
	}
	if batchHits.Load() != 1 {
		t.Fatalf("batch calls = %d, want 1", batchHits.Load())
	}
	if singleHits.Load() != 1 {
		t.Fatalf("stage-two calls = %d, want 1 (only the high scorer)", singleHits.Load())
	}
}
