package pipeline

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"job-search-automation/internal/scorer"
)

// fakeExtractor implements JobExtractor, counting calls.
type fakeExtractor struct {
	calls atomic.Int32
	fn    func(job scorer.Job) (string, error)
}

func (f *fakeExtractor) ExtractJob(_ context.Context, job scorer.Job) (string, error) {
	f.calls.Add(1)
	if f.fn != nil {
		return f.fn(job)
	}
	return `{"field_function":"engineering","summary":"ok"}`, nil
}

// ExtractPending must extract each pending job's global row once: a second
// pass over the same backlog finds every extract already present and spends
// zero calls. This is the cross-tenant dedup property in miniature, since
// later tenants hit the same global table the first pass filled.
func TestExtractPendingExtractsOnce(t *testing.T) {
	repo, _ := newRepo(t)
	insertLead(t, repo, "j1", "Platform Engineer")
	insertLead(t, repo, "j2", "SRE")

	ex := &fakeExtractor{}
	cfg := ExtractConfig{DailyLimit: 100, Concurrency: 2}

	out, err := ExtractPending(context.Background(), repo, ex, cfg, discardLogger())
	if err != nil {
		t.Fatalf("ExtractPending: %v", err)
	}
	if out.Extracted != 2 || out.Failed != 0 {
		t.Fatalf("result = %+v, want 2 extracted", out)
	}
	if n, err := repo.CountExtractsToday(); err != nil || n != 2 {
		t.Fatalf("CountExtractsToday = %d (%v), want 2", n, err)
	}

	// Second pass: everything cached, no calls.
	out, err = ExtractPending(context.Background(), repo, ex, cfg, discardLogger())
	if err != nil {
		t.Fatalf("second ExtractPending: %v", err)
	}
	if out.Extracted != 0 || ex.calls.Load() != 2 {
		t.Fatalf("second pass extracted %d with %d total calls, want 0 and 2", out.Extracted, ex.calls.Load())
	}
}

// The daily budget must bound a pass and carry across passes via the table
// ledger: limit 1 extracts one job now and the second only after "tomorrow"
// (not simulated here; the second pass gets zero budget).
func TestExtractPendingRespectsDailyLimit(t *testing.T) {
	repo, _ := newRepo(t)
	insertLead(t, repo, "j1", "Platform Engineer")
	insertLead(t, repo, "j2", "SRE")

	ex := &fakeExtractor{}
	out, err := ExtractPending(context.Background(), repo, ex, ExtractConfig{DailyLimit: 1, Concurrency: 2}, discardLogger())
	if err != nil {
		t.Fatalf("ExtractPending: %v", err)
	}
	if out.Extracted != 1 {
		t.Fatalf("extracted = %d, want 1", out.Extracted)
	}
	out, err = ExtractPending(context.Background(), repo, ex, ExtractConfig{DailyLimit: 1, Concurrency: 2}, discardLogger())
	if err != nil {
		t.Fatalf("second ExtractPending: %v", err)
	}
	if out.Extracted != 0 || ex.calls.Load() != 1 {
		t.Fatalf("budget not enforced: second pass extracted %d, %d total calls", out.Extracted, ex.calls.Load())
	}
}

// A failed extraction must not poison the cache: the job stays a pending
// target and is retried on the next pass.
func TestExtractPendingRetriesFailures(t *testing.T) {
	repo, _ := newRepo(t)
	insertLead(t, repo, "j1", "Platform Engineer")

	fail := &fakeExtractor{fn: func(scorer.Job) (string, error) { return "", errors.New("boom") }}
	out, err := ExtractPending(context.Background(), repo, fail, ExtractConfig{DailyLimit: 10, Concurrency: 1}, discardLogger())
	if err != nil {
		t.Fatalf("ExtractPending: %v", err)
	}
	if out.Failed != 1 || out.Extracted != 0 {
		t.Fatalf("result = %+v, want 1 failed", out)
	}

	ok := &fakeExtractor{}
	out, err = ExtractPending(context.Background(), repo, ok, ExtractConfig{DailyLimit: 10, Concurrency: 1}, discardLogger())
	if err != nil {
		t.Fatalf("retry ExtractPending: %v", err)
	}
	if out.Extracted != 1 {
		t.Fatalf("retry extracted = %d, want 1", out.Extracted)
	}
}
