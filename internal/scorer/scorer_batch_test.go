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
	t.Run("well formed maps by job_id and clamps", func(t *testing.T) {
		text := `[{"job_id":"j0","score":8,"reasoning":"a"},{"job_id":"j1","score":42,"reasoning":"b"},{"job_id":"j2","score":0,"reasoning":"c"}]`
		got := parseBatchResponse(text, 3)
		if len(got) != 3 {
			t.Fatalf("got %d results, want 3", len(got))
		}
		if got[0].Score == nil || *got[0].Score != 8 {
			t.Fatalf("job j0 score = %v, want 8", ptrStr(got[0].Score))
		}
		if got[1].Score == nil || *got[1].Score != 10 {
			t.Fatalf("job j1 score = %v, want 10 (clamped)", ptrStr(got[1].Score))
		}
		if got[2].Score == nil || *got[2].Score != 1 {
			t.Fatalf("job j2 score = %v, want 1 (clamped)", ptrStr(got[2].Score))
		}
	})

	t.Run("drops out-of-range, unknown-format, and missing ids", func(t *testing.T) {
		text := `[{"job_id":"j0","score":7,"reasoning":"a"},{"job_id":"j9","score":7,"reasoning":"x"},{"job_id":"banana","score":7,"reasoning":"y"},{"job_id":"","score":7,"reasoning":"z"},{"score":7,"reasoning":"w"}]`
		got := parseBatchResponse(text, 3)
		if _, ok := got[0]; !ok {
			t.Fatal("job j0 missing")
		}
		if len(got) != 1 {
			t.Fatalf("got %d results, want only j0 (out-of-range and unknown ids dropped)", len(got))
		}
		if _, ok := got[1]; ok {
			t.Fatal("job j1 was never returned; should be absent")
		}
	})

	t.Run("duplicate job_id keeps the first entry", func(t *testing.T) {
		text := `[{"job_id":"j0","score":8,"reasoning":"first"},{"job_id":"j0","score":2,"reasoning":"second"}]`
		got := parseBatchResponse(text, 2)
		if len(got) != 1 {
			t.Fatalf("got %d results, want 1", len(got))
		}
		if got[0].Score == nil || *got[0].Score != 8 || got[0].Reasoning != "first" {
			t.Fatalf("duplicate id should keep the first entry, got %v %q", ptrStr(got[0].Score), got[0].Reasoning)
		}
	})

	t.Run("tolerates code fences", func(t *testing.T) {
		text := "```json\n[{\"job_id\":\"j0\",\"score\":6,\"reasoning\":\"a\"}]\n```"
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

// The prompts must keep their content ordered most-stable-first so Gemini's
// implicit prefix caching gets hits: static rubric before the per-tenant
// resume, resume before the per-call job block(s).
func TestScoringPromptsKeepVariableJobsLast(t *testing.T) {
	single := buildScorePrompt("resume", 4.0, Job{Title: "variable job"})
	if strings.Index(single, "STEP 1") > strings.Index(single, "## Candidate Resume") {
		t.Fatalf("single prompt puts rubric after the per-tenant resume")
	}
	if strings.Index(single, "## Candidate Resume") > strings.Index(single, "## Job Listing") {
		t.Fatalf("single prompt puts resume after the variable job listing")
	}
	if !strings.HasSuffix(single, "No description available.") {
		t.Fatalf("single prompt should end with the variable job block, got tail %q", single[len(single)-40:])
	}

	batch := buildBatchPrompt("resume", 4.0, []Job{{Title: "a"}, {Title: "b"}})
	if strings.Index(batch, "STEP 1") > strings.Index(batch, "## Candidate Resume") {
		t.Fatalf("batch prompt puts rubric after the per-tenant resume")
	}
	if strings.Index(batch, "## Candidate Resume") > strings.Index(batch, "## Job Listings") {
		t.Fatalf("batch prompt puts resume after variable job listings")
	}
	if !strings.Contains(batch, "Include every job exactly once.\n\n## Candidate Resume") {
		t.Fatalf("batch prompt should declare output contract before the resume and listings")
	}
	if isolation := strings.Index(batch, "COMPLETE ISOLATION"); isolation < 0 || isolation > strings.Index(batch, "## Candidate Resume") {
		t.Fatalf("batch prompt must state the isolation rule inside the static prefix")
	}
	if !strings.Contains(batch, "### Job j0\n") || !strings.Contains(batch, "### Job j1\n") {
		t.Fatalf("batch prompt job blocks must carry j<N> headings for job_id-keyed output")
	}

	// The static prefix (everything before the resume) must be byte-identical
	// across tenants and calls; a drifting prefix silently kills cache hits.
	singlePrefix := single[:strings.Index(single, "## Candidate Resume")]
	other := buildScorePrompt("a different resume", 4.0, Job{Title: "another job"})
	if otherPrefix := other[:strings.Index(other, "## Candidate Resume")]; otherPrefix != singlePrefix {
		t.Fatalf("single prompt static prefix varies across calls")
	}
	batchPrefix := batch[:strings.Index(batch, "## Candidate Resume")]
	otherBatch := buildBatchPrompt("a different resume", 4.0, []Job{{Title: "c"}, {Title: "d"}, {Title: "e"}})
	if otherPrefix := otherBatch[:strings.Index(otherBatch, "## Candidate Resume")]; otherPrefix != batchPrefix {
		t.Fatalf("batch prompt static prefix varies across calls")
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
			// Return only jobs j0 and j2; drop j1 to force reconciliation.
			writeCandidate(w, `[{"job_id":"j0","score":8,"reasoning":"a"},{"job_id":"j2","score":9,"reasoning":"c"}]`)
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
			writeCandidate(w, `[{"job_id":"j0","score":3,"reasoning":"a"},{"job_id":"j1","score":3,"reasoning":"b"}]`)
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
			// j0 scores high (>= threshold) so it must be re-scored; j1 scores
			// low and must be left as-is.
			writeCandidate(w, `[{"job_id":"j0","score":9,"reasoning":"batch high"},{"job_id":"j1","score":4,"reasoning":"batch low"}]`)
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

func TestFormatJobBlockCapsDescription(t *testing.T) {
	long := strings.Repeat("x", scoreDescriptionLength+5000)
	block := formatJobBlock(Job{Title: "t", Company: "c", Description: long})
	if strings.Contains(block, long) {
		t.Fatal("description was not truncated")
	}
	if !strings.Contains(block, long[:scoreDescriptionLength]) {
		t.Fatal("truncated description missing from block")
	}
	if len(block) > scoreDescriptionLength+500 {
		t.Fatalf("block length = %d, want about %d", len(block), scoreDescriptionLength)
	}
}
