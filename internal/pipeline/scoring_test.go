package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"job-search-automation/internal/db"
	"job-search-automation/internal/model"
	"job-search-automation/internal/scorer"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeScorer implements JobScorer with a caller-supplied per-job function.
// calls counts jobs scored; batchCalls counts ScoreJobs invocations (batches).
type fakeScorer struct {
	mu         sync.Mutex
	calls      int
	batchCalls int
	fn         func(scorer.Job) (scorer.Result, error)
}

func (f *fakeScorer) scoreOne(job scorer.Job) (scorer.Result, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.fn(job)
}

// ScoreJobs mirrors the real contract: a per-job error fails the whole batch.
func (f *fakeScorer) ScoreJobs(_ context.Context, jobs []scorer.Job) ([]scorer.Result, error) {
	f.mu.Lock()
	f.batchCalls++
	f.mu.Unlock()
	out := make([]scorer.Result, len(jobs))
	for i, job := range jobs {
		r, err := f.scoreOne(job)
		if err != nil {
			return nil, err
		}
		out[i] = r
	}
	return out, nil
}

func newRepo(t *testing.T) (*db.Repository, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.db")
	repo, err := db.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { repo.Close() })
	return repo, path
}

func insertLead(t *testing.T, repo *db.Repository, id, title string) {
	t.Helper()
	lead := model.Lead{
		JobLead: model.JobLead{
			Title:            title,
			Company:          "Co",
			Description:      "desc",
			DirectApplyURL:   "https://example.com/" + id,
			ATSPlatformName:  "Greenhouse",
			ScrapedTimestamp: "2026-06-07T00:00:00.000Z",
			Location:         "Remote",
			PostedAt:         "2026-06-06",
		},
		ID: id,
	}
	if _, err := repo.InsertScrapedLead(lead); err != nil {
		t.Fatalf("insert lead %s: %v", id, err)
	}
}

func res(score int, reason string) scorer.Result {
	return scorer.Result{Score: &score, Reasoning: reason}
}

// verifyConn opens a second read connection for assertions.
func verifyConn(t *testing.T, path string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("verify open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestScoreUnscoredScoresAndArchives(t *testing.T) {
	repo, path := newRepo(t)
	insertLead(t, repo, "j1", "Platform Engineer")
	insertLead(t, repo, "j2", "low")
	insertLead(t, repo, "j3", "SRE")

	fake := &fakeScorer{fn: func(j scorer.Job) (scorer.Result, error) {
		if j.Title == "low" {
			return res(3, "weak"), nil
		}
		return res(8, "strong"), nil
	}}

	out, err := ScoreUnscored(context.Background(), repo, fake,
		ScoreConfig{DailyLimit: 500, Concurrency: 2, ArchiveThreshold: 4}, discardLogger())
	if err != nil {
		t.Fatalf("ScoreUnscored: %v", err)
	}
	if out.ScoredOK != 3 || out.ScoredFailed != 0 {
		t.Fatalf("result = %+v, want 3 ok / 0 failed", out)
	}

	conn := verifyConn(t, path)
	// j2 (score 3 <= threshold 4) archived; the others remain pending with scores.
	if got := scalarStr(t, conn, "SELECT status FROM jobs WHERE id='j2'"); got != "archived" {
		t.Fatalf("j2 status = %q, want archived", got)
	}
	if got := scalarStr(t, conn, "SELECT status FROM jobs WHERE id='j1'"); got != "pending" {
		t.Fatalf("j1 status = %q, want pending", got)
	}
	if got := scalarInt(t, conn, "SELECT score FROM jobs WHERE id='j1'"); got != 8 {
		t.Fatalf("j1 score = %d, want 8", got)
	}
	if n := scalarInt(t, conn, "SELECT COUNT(*) FROM jobs WHERE score IS NULL"); n != 0 {
		t.Fatalf("%d jobs left unscored, want 0", n)
	}
}

func TestScoreUnscoredBatchesCalls(t *testing.T) {
	repo, path := newRepo(t)
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		insertLead(t, repo, id, "Engineer "+id)
	}

	fake := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(7, "ok"), nil }}
	out, err := ScoreUnscored(context.Background(), repo, fake,
		ScoreConfig{DailyLimit: 500, Concurrency: 2, ArchiveThreshold: 4, BatchSize: 4}, discardLogger())
	if err != nil {
		t.Fatalf("ScoreUnscored: %v", err)
	}
	// 10 jobs at batch size 4 => 3 batches (4 + 4 + 2), all 10 scored.
	if fake.batchCalls != 3 {
		t.Fatalf("batch calls = %d, want 3 (ceil(10/4))", fake.batchCalls)
	}
	if fake.calls != 10 {
		t.Fatalf("jobs scored = %d, want 10", fake.calls)
	}
	if out.ScoredOK != 10 {
		t.Fatalf("result.ScoredOK = %d, want 10", out.ScoredOK)
	}
	conn := verifyConn(t, path)
	if n := scalarInt(t, conn, "SELECT COUNT(*) FROM jobs WHERE score IS NULL"); n != 0 {
		t.Fatalf("%d jobs left unscored, want 0", n)
	}
}

func TestScoreUnscoredRespectsDailyCap(t *testing.T) {
	repo, _ := newRepo(t)
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		insertLead(t, repo, id, "Engineer "+id)
	}
	// Pre-spend usage so only 5 remain: 500 - 485 - 10 (reserve) = 5.
	for i := 0; i < 485; i++ {
		if err := repo.RecordAPICall(scorer.Model); err != nil {
			t.Fatalf("seed usage: %v", err)
		}
	}

	fake := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(7, "ok"), nil }}
	out, err := ScoreUnscored(context.Background(), repo, fake,
		ScoreConfig{DailyLimit: 500, Concurrency: 3, ArchiveThreshold: 4}, discardLogger())
	if err != nil {
		t.Fatalf("ScoreUnscored: %v", err)
	}
	if fake.calls != 5 {
		t.Fatalf("scored %d jobs, want 5 (capped by remaining quota)", fake.calls)
	}
	if out.Scored != 5 {
		t.Fatalf("result.Scored = %d, want 5", out.Scored)
	}
}

func TestScoreUnscoredRespectsHostKeyCap(t *testing.T) {
	repo, _ := newRepo(t)
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		insertLead(t, repo, id, "Engineer "+id)
	}
	// The tenant's own daily tally is empty, but the shared host key is nearly
	// spent: 100 - 87 - 10 (reserve) = 3 host calls remain.
	for i := 0; i < 87; i++ {
		if err := repo.RecordHostAPICall(scorer.Model); err != nil {
			t.Fatalf("seed host usage: %v", err)
		}
	}

	fake := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(7, "ok"), nil }}
	out, err := ScoreUnscored(context.Background(), repo, fake,
		ScoreConfig{DailyLimit: 500, Concurrency: 3, ArchiveThreshold: 4,
			HostKey: true, HostDailyLimit: 100}, discardLogger())
	if err != nil {
		t.Fatalf("ScoreUnscored: %v", err)
	}
	// Clamped by the host key's remaining budget (3), not the per-tenant limit.
	if fake.calls != 3 {
		t.Fatalf("scored %d jobs, want 3 (capped by host-key remaining)", fake.calls)
	}
	if out.Scored != 3 {
		t.Fatalf("result.Scored = %d, want 3", out.Scored)
	}
}

func TestScoreUnscoredRespectsPerTenantHostCap(t *testing.T) {
	repo, _ := newRepo(t)
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		insertLead(t, repo, id, "Engineer "+id)
	}
	// Shared host key has plenty left (500), but the per-tenant host cap is 15:
	// 15 - 0 used - 10 reserve = 5 calls this tenant may make today.
	fake := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(7, "ok"), nil }}
	out, err := ScoreUnscored(context.Background(), repo, fake,
		ScoreConfig{DailyLimit: 500, Concurrency: 3, ArchiveThreshold: 4,
			HostKey: true, HostDailyLimit: 500, HostPerTenantDailyLimit: 15}, discardLogger())
	if err != nil {
		t.Fatalf("ScoreUnscored: %v", err)
	}
	if fake.calls != 5 {
		t.Fatalf("scored %d jobs, want 5 (capped by per-tenant host limit)", fake.calls)
	}
	if out.Scored != 5 {
		t.Fatalf("result.Scored = %d, want 5", out.Scored)
	}
}

// TestScoreUnscoredInactiveDailyLimitClampsKeepWarm is the regression guard for
// idle tenants draining a shared key. MaxJobs caps one pass, so with many passes
// a day an inactive tenant could spend far past its share; InactiveDailyLimit is
// the daily bound that stops it. Here the ordinary per-tenant cap (150) would
// allow 140 calls, but the keep-warm budget (30) allows 20.
func TestScoreUnscoredInactiveDailyLimitClampsKeepWarm(t *testing.T) {
	repo, _ := newRepo(t)
	for i := 0; i < 40; i++ {
		insertLead(t, repo, string(rune('a'+i%26))+strconv.Itoa(i), "Engineer")
	}
	// Simulate earlier passes today: 5 calls already spent, so 30 - 5 - 10 = 15 left.
	for i := 0; i < 5; i++ {
		if err := repo.RecordAPICall(scorer.Model); err != nil {
			t.Fatalf("seed tenant usage: %v", err)
		}
	}
	fake := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(7, "ok"), nil }}
	out, err := ScoreUnscored(context.Background(), repo, fake,
		ScoreConfig{DailyLimit: 500, Concurrency: 3, ArchiveThreshold: 4,
			HostKey: true, HostDailyLimit: 500, HostPerTenantDailyLimit: 150,
			MaxJobs: 100, InactiveDailyLimit: 30}, discardLogger())
	if err != nil {
		t.Fatalf("ScoreUnscored: %v", err)
	}
	if out.Scored != 15 {
		t.Fatalf("result.Scored = %d, want 15 (clamped by inactive keep-warm daily limit)", out.Scored)
	}
}

// TestScoreUnscoredInactiveDailyLimitNeverLoosens guards the direction of the
// clamp: keep-warm may only tighten a tenant's budget. A value above the
// ordinary per-tenant cap must not raise it.
func TestScoreUnscoredInactiveDailyLimitNeverLoosens(t *testing.T) {
	repo, _ := newRepo(t)
	for i := 0; i < 30; i++ {
		insertLead(t, repo, string(rune('a'+i%26))+strconv.Itoa(i), "Engineer")
	}
	fake := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(7, "ok"), nil }}
	out, err := ScoreUnscored(context.Background(), repo, fake,
		ScoreConfig{DailyLimit: 500, Concurrency: 3, ArchiveThreshold: 4,
			HostKey: true, HostDailyLimit: 500, HostPerTenantDailyLimit: 15,
			InactiveDailyLimit: 400}, discardLogger())
	if err != nil {
		t.Fatalf("ScoreUnscored: %v", err)
	}
	if out.Scored != 5 {
		t.Fatalf("result.Scored = %d, want 5 (per-tenant cap still binds)", out.Scored)
	}
}

// TestScoreUnscoredInactiveDailyLimitAppliesWithoutPerTenantCap covers the
// self-host-style config where no per-tenant cap is set: the keep-warm budget
// must still bind on its own.
func TestScoreUnscoredInactiveDailyLimitAppliesWithoutPerTenantCap(t *testing.T) {
	repo, _ := newRepo(t)
	for i := 0; i < 30; i++ {
		insertLead(t, repo, string(rune('a'+i%26))+strconv.Itoa(i), "Engineer")
	}
	fake := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(7, "ok"), nil }}
	out, err := ScoreUnscored(context.Background(), repo, fake,
		ScoreConfig{DailyLimit: 500, Concurrency: 3, ArchiveThreshold: 4,
			HostKey: true, HostDailyLimit: 500, HostPerTenantDailyLimit: 0,
			InactiveDailyLimit: 25}, discardLogger())
	if err != nil {
		t.Fatalf("ScoreUnscored: %v", err)
	}
	if out.Scored != 15 {
		t.Fatalf("result.Scored = %d, want 15 (keep-warm budget binds alone)", out.Scored)
	}
}

func TestScoreUnscoredHostKeyIgnoresGenericDailyCap(t *testing.T) {
	repo, _ := newRepo(t)
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		insertLead(t, repo, id, "Engineer "+id)
	}
	// Hosted shared-key traffic may include discovery, canonicalization, market
	// research, and scoring. A tenant can be above the generic self-host cap while
	// the hosted host-key and per-tenant budgets still have room.
	for i := 0; i < 638; i++ {
		if err := repo.RecordAPICall(scorer.Model); err != nil {
			t.Fatalf("seed tenant usage: %v", err)
		}
	}
	for i := 0; i < 336; i++ {
		if err := repo.RecordHostAPICall(scorer.Model); err != nil {
			t.Fatalf("seed host usage: %v", err)
		}
	}

	fake := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(7, "ok"), nil }}
	out, err := ScoreUnscored(context.Background(), repo, fake,
		ScoreConfig{DailyLimit: 500, Concurrency: 3, ArchiveThreshold: 4,
			HostKey: true, HostDailyLimit: 1000, HostPerTenantDailyLimit: 660}, discardLogger())
	if err != nil {
		t.Fatalf("ScoreUnscored: %v", err)
	}
	if fake.calls != 6 {
		t.Fatalf("scored %d jobs, want 6 (host-key caps still have budget)", fake.calls)
	}
	if out.Scored != 6 {
		t.Fatalf("result.Scored = %d, want 6", out.Scored)
	}
}

// TestScoreUnscoredPerTenantCapDisabledByDefault guards the self-host invariant:
// HostPerTenantDailyLimit <= 0 must impose no extra clamp, so a host-key run scores
// up to the full DailyLimit just as it did before the cap existed.
func TestScoreUnscoredPerTenantCapDisabledByDefault(t *testing.T) {
	repo, _ := newRepo(t)
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		insertLead(t, repo, id, "Engineer "+id)
	}
	fake := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(7, "ok"), nil }}
	out, err := ScoreUnscored(context.Background(), repo, fake,
		ScoreConfig{DailyLimit: 500, Concurrency: 3, ArchiveThreshold: 4,
			HostKey: true, HostDailyLimit: 500, HostPerTenantDailyLimit: 0}, discardLogger())
	if err != nil {
		t.Fatalf("ScoreUnscored: %v", err)
	}
	if out.Scored != 6 {
		t.Fatalf("result.Scored = %d, want 6 (no per-tenant clamp when disabled)", out.Scored)
	}
}

func TestScoreUnscoredStopsWhenHostKeyExhausted(t *testing.T) {
	repo, _ := newRepo(t)
	insertLead(t, repo, "a", "Engineer a")
	// Host key fully spent (>= limit - reserve), tenant tally still empty.
	for i := 0; i < 100; i++ {
		if err := repo.RecordHostAPICall(scorer.Model); err != nil {
			t.Fatalf("seed host usage: %v", err)
		}
	}

	fake := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(7, "ok"), nil }}
	out, err := ScoreUnscored(context.Background(), repo, fake,
		ScoreConfig{DailyLimit: 500, Concurrency: 3, ArchiveThreshold: 4,
			HostKey: true, HostDailyLimit: 100}, discardLogger())
	if err != nil {
		t.Fatalf("ScoreUnscored: %v", err)
	}
	if fake.calls != 0 || out.Scored != 0 {
		t.Fatalf("scored %d (result %d), want 0: host key exhausted", fake.calls, out.Scored)
	}
}

func TestScoreUnscoredDrainsOnQuotaError(t *testing.T) {
	repo, path := newRepo(t)
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		insertLead(t, repo, id, "Engineer "+id)
	}

	fake := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) {
		return scorer.Result{}, errors.New("RESOURCE_EXHAUSTED: quota gone")
	}}
	// Concurrency 1 makes the drain deterministic: the first failure trips the
	// quota flag before any further job is dispatched.
	out, err := ScoreUnscored(context.Background(), repo, fake,
		ScoreConfig{DailyLimit: 500, Concurrency: 1, ArchiveThreshold: 4}, discardLogger())
	if err != nil {
		t.Fatalf("ScoreUnscored: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("scored %d jobs after a quota error, want 1 then drain", fake.calls)
	}
	if out.ScoredFailed != 1 {
		t.Fatalf("result = %+v, want 1 failed", out)
	}

	conn := verifyConn(t, path)
	// The failed job recorded its error; the rest are untouched (still unscored).
	if got := scalarStr(t, conn, "SELECT COALESCE(score_error,'') FROM jobs WHERE id='a'"); got == "" {
		t.Fatal("job a should have a score_error recorded")
	}
	if n := scalarInt(t, conn, "SELECT COUNT(*) FROM jobs WHERE score IS NULL"); n != 6 {
		t.Fatalf("%d jobs unscored, want all 6 (none successfully scored)", n)
	}
}

// alwaysStrong scores every job 8 so nothing is archived, isolating the count.
func alwaysStrong() *fakeScorer {
	return &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(8, "strong"), nil }}
}

func scalarStr(t *testing.T, conn *sql.DB, q string) string {
	t.Helper()
	var s string
	if err := conn.QueryRow(q).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return s
}

func scalarInt(t *testing.T, conn *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

func TestScoreUnscoredMaxJobsCap(t *testing.T) {
	repo, _ := newRepo(t)
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		insertLead(t, repo, id, "Engineer "+id)
	}

	// Quota would allow all 10; the keep-warm cap clamps the pull to 4.
	fake := &fakeScorer{fn: func(scorer.Job) (scorer.Result, error) { return res(7, "ok"), nil }}
	out, err := ScoreUnscored(context.Background(), repo, fake,
		ScoreConfig{DailyLimit: 500, Concurrency: 3, ArchiveThreshold: 4, MaxJobs: 4}, discardLogger())
	if err != nil {
		t.Fatalf("ScoreUnscored: %v", err)
	}
	if fake.calls != 4 {
		t.Fatalf("scored %d jobs, want 4 (keep-warm cap)", fake.calls)
	}
	if out.Scored != 4 {
		t.Fatalf("result.Scored = %d, want 4", out.Scored)
	}
}

// TestHostQuotaAvailable covers the gate that keeps non-scoring consumers (the
// market research cron) off an exhausted shared key. Before this existed the
// cron fanned out per tenant after the wall and burned a 429 retry ladder each.
func TestHostQuotaAvailable(t *testing.T) {
	repo, _ := newRepo(t)
	// 495 spent against a 500 ceiling leaves 5, which the 10-call reserve consumes.
	for i := 0; i < 495; i++ {
		if err := repo.RecordHostAPICall(scorer.Model); err != nil {
			t.Fatalf("seed host usage: %v", err)
		}
	}
	ok, used, err := HostQuotaAvailable(repo, 500)
	if err != nil {
		t.Fatalf("HostQuotaAvailable: %v", err)
	}
	if used != 495 {
		t.Fatalf("hostUsed = %d, want 495", used)
	}
	if ok {
		t.Fatal("want blocked: 500 - 485 - quotaReserve is not positive")
	}
	// The same tally against the old, wrong 2000 ceiling still has room, which is
	// exactly why prod kept firing into the provider's real 500/day wall.
	if ok, _, err = HostQuotaAvailable(repo, 2000); err != nil || !ok {
		t.Fatalf("want allowed under a 2000 ceiling, got ok=%v err=%v", ok, err)
	}
	// An unconfigured ceiling (self-host) must not block the caller.
	if ok, _, err = HostQuotaAvailable(repo, 0); err != nil || !ok {
		t.Fatalf("want allowed when the host ceiling is unset, got ok=%v err=%v", ok, err)
	}
}
