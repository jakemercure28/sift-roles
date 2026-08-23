package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"job-search-automation/internal/db"
	"job-search-automation/internal/scorer"
)

// fakeBatchAPI is an in-memory Gemini Batch API: submits are captured, and
// polls answer from a scripted map.
type fakeBatchAPI struct {
	submits   []fakeSubmit
	statuses  map[string]scorer.BatchStatus
	submitErr error
	pollErrs  map[string]error
	nextName  int
}

type fakeSubmit struct {
	name  string
	items []scorer.BatchItem
}

func (f *fakeBatchAPI) SubmitScoreBatch(_ context.Context, _ string, items []scorer.BatchItem) (string, error) {
	if f.submitErr != nil {
		return "", f.submitErr
	}
	f.nextName++
	name := fmt.Sprintf("batches/fake-%d", f.nextName)
	f.submits = append(f.submits, fakeSubmit{name: name, items: items})
	return name, nil
}

func (f *fakeBatchAPI) GetScoreBatch(_ context.Context, name string) (scorer.BatchStatus, error) {
	if err := f.pollErrs[name]; err != nil {
		return scorer.BatchStatus{}, err
	}
	st, ok := f.statuses[name]
	if !ok {
		return scorer.BatchStatus{Name: name, State: scorer.BatchStateRunning}, nil
	}
	return st, nil
}

// fakePromptScorer builds trivially inspectable prompts and parses the sync
// SCORE/REASONING format via a real Scorer-free stub.
type fakePromptScorer struct{}

func (fakePromptScorer) ScorePrompt(job scorer.Job) (string, error) {
	return "PROMPT:" + job.Title, nil
}

func (fakePromptScorer) ParseResult(_ scorer.Job, text string) scorer.Result {
	var score int
	var reasoning string
	if _, err := fmt.Sscanf(text, "SCORE: %d", &score); err != nil {
		return scorer.Result{Reasoning: "Score parse failed. Raw: " + text}
	}
	if i := strings.Index(text, "REASONING: "); i >= 0 {
		reasoning = text[i+len("REASONING: "):]
	}
	return scorer.Result{Score: &score, Reasoning: reasoning}
}

func batchTestTenant(repo *db.Repository) BatchTenant {
	return BatchTenant{
		UserID:     repo.UserID(),
		Repo:       repo,
		Scorer:     fakePromptScorer{},
		ResumeHash: "hash-submit",
	}
}

func seedUnscored(t *testing.T, repo *db.Repository, id, title, desc string) {
	t.Helper()
	if _, err := repo.RawDB().Exec(
		`INSERT INTO jobs (id, title, company, location, description, status)
		 VALUES (?, ?, 'Co', 'Remote', ?, 'pending')`, id, title, desc); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// Submit must pull eligible jobs (description-less ones never leave SQL),
// submit one batch per tenant under the gates, and persist the ledger so the
// members leave the sync queue.
func TestSubmitScoreBatches(t *testing.T) {
	repo, _ := newRepo(t)
	seedUnscored(t, repo, "j1", "DevOps", "a real description")
	seedUnscored(t, repo, "j2", "SRE", "another description")
	seedUnscored(t, repo, "j-nodesc", "Bare Title", "")

	api := &fakeBatchAPI{}
	cfg := BatchScoreConfig{MaxOpenBatches: 4, DailyJobLimit: 100, MaxJobsPerSubmit: 10, ArchiveThreshold: 3}
	n, err := SubmitScoreBatches(context.Background(), repo, api, []BatchTenant{batchTestTenant(repo)}, cfg, discardLogger())
	if err != nil {
		t.Fatalf("SubmitScoreBatches: %v", err)
	}
	if n != 2 || len(api.submits) != 1 || len(api.submits[0].items) != 2 {
		t.Fatalf("submitted=%d submits=%+v", n, api.submits)
	}
	if api.submits[0].items[0].Prompt != "PROMPT:DevOps" {
		t.Fatalf("prompt = %q", api.submits[0].items[0].Prompt)
	}

	// Members left the sync queue, and the no-description row never entered
	// it (excluded in SQL, not stamped): it sits as a plain pending row until
	// a later scrape fills the body, at which point it scores normally.
	unscored, err := repo.GetUnscoredJobs(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(unscored) != 0 {
		t.Fatalf("sync queue = %+v, want empty", unscored)
	}
	var se *string
	if err := repo.RawDB().QueryRow(`SELECT score_error FROM jobs WHERE id='j-nodesc'`).Scan(&se); err != nil {
		t.Fatal(err)
	}
	if se != nil {
		t.Fatalf("no-desc score_error = %q, want NULL (nothing stamps it anymore)", *se)
	}

	// A second pass submits nothing: everything eligible is already in flight.
	n, err = SubmitScoreBatches(context.Background(), repo, api, []BatchTenant{batchTestTenant(repo)}, cfg, discardLogger())
	if err != nil || n != 0 {
		t.Fatalf("second pass = (%d, %v), want (0, nil)", n, err)
	}
}

// A quota-exhausted submit surfaces the typed error so the orchestrator can
// start its cooldown; any other submit failure keeps the old log-and-swallow
// contract (stop the pass, return nil).
func TestSubmitScoreBatchesQuotaError(t *testing.T) {
	cfg := BatchScoreConfig{MaxOpenBatches: 4, DailyJobLimit: 100, MaxJobsPerSubmit: 10, ArchiveThreshold: 3}

	repo, _ := newRepo(t)
	seedUnscored(t, repo, "q1", "DevOps", "desc")
	api := &fakeBatchAPI{submitErr: fmt.Errorf("%w: quota exceeded", scorer.ErrQuotaExhausted)}
	n, err := SubmitScoreBatches(context.Background(), repo, api, []BatchTenant{batchTestTenant(repo)}, cfg, discardLogger())
	if !errors.Is(err, scorer.ErrQuotaExhausted) || n != 0 {
		t.Fatalf("quota submit = (%d, %v), want (0, ErrQuotaExhausted)", n, err)
	}

	repo2, _ := newRepo(t)
	seedUnscored(t, repo2, "q2", "SRE", "desc")
	api2 := &fakeBatchAPI{submitErr: errors.New("transport blip")}
	n, err = SubmitScoreBatches(context.Background(), repo2, api2, []BatchTenant{batchTestTenant(repo2)}, cfg, discardLogger())
	if err != nil || n != 0 {
		t.Fatalf("plain-failure submit = (%d, %v), want (0, nil)", n, err)
	}
}

// The global gates must stop submission: open-batch cap and daily job budget.
func TestSubmitScoreBatchesGates(t *testing.T) {
	repo, _ := newRepo(t)
	seedUnscored(t, repo, "g1", "DevOps", "desc")
	seedUnscored(t, repo, "g2", "SRE", "desc")

	// Daily budget of 1: only one job fits.
	api := &fakeBatchAPI{}
	cfg := BatchScoreConfig{MaxOpenBatches: 4, DailyJobLimit: 1, MaxJobsPerSubmit: 10, ArchiveThreshold: 3}
	n, err := SubmitScoreBatches(context.Background(), repo, api, []BatchTenant{batchTestTenant(repo)}, cfg, discardLogger())
	if err != nil || n != 1 {
		t.Fatalf("budgeted submit = (%d, %v), want 1", n, err)
	}

	// Open-batch cap already reached: nothing submits.
	cfg2 := BatchScoreConfig{MaxOpenBatches: 1, DailyJobLimit: 100, MaxJobsPerSubmit: 10, ArchiveThreshold: 3}
	n, err = SubmitScoreBatches(context.Background(), repo, api, []BatchTenant{batchTestTenant(repo)}, cfg2, discardLogger())
	if err != nil || n != 0 {
		t.Fatalf("capped submit = (%d, %v), want 0", n, err)
	}
}

// Poll must apply a finished batch through the guarded write (auto-archiving
// low scores, stamping the submit-time hash), close it, and free membership;
// running batches stay open; expired batches free their jobs unscored.
func TestPollScoreBatches(t *testing.T) {
	repo, _ := newRepo(t)
	seedUnscored(t, repo, "p-high", "DevOps", "desc")
	seedUnscored(t, repo, "p-low", "SRE", "desc")
	seedUnscored(t, repo, "p-raced", "Platform", "desc")
	seedUnscored(t, repo, "p-expired", "Infra", "desc")

	api := &fakeBatchAPI{statuses: map[string]scorer.BatchStatus{}}
	cfg := BatchScoreConfig{MaxOpenBatches: 4, DailyJobLimit: 100, MaxJobsPerSubmit: 3, ArchiveThreshold: 3}
	tenant := batchTestTenant(repo)

	// First batch takes the 3 newest; second takes the leftover.
	if _, err := SubmitScoreBatches(context.Background(), repo, api, []BatchTenant{tenant}, cfg, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if _, err := SubmitScoreBatches(context.Background(), repo, api, []BatchTenant{tenant}, cfg, discardLogger()); err != nil {
		t.Fatal(err)
	}
	if len(api.submits) != 2 {
		t.Fatalf("submits = %d, want 2", len(api.submits))
	}

	// Locate which submit holds which job, then script the poll answers.
	memberOf := map[string]string{}
	for _, s := range api.submits {
		for _, it := range s.items {
			memberOf[it.Key] = s.name
		}
	}
	first := api.submits[0].name
	second := api.submits[1].name

	// The user archived p-raced while the batch was in flight.
	if _, err := repo.RawDB().Exec(`UPDATE jobs SET status='archived' WHERE id='p-raced'`); err != nil {
		t.Fatal(err)
	}

	results := []scorer.BatchResult{}
	for _, key := range []string{"p-high", "p-low", "p-raced", "p-expired"} {
		if memberOf[key] != first {
			continue
		}
		text := "SCORE: 8\nREASONING: strong"
		if key == "p-low" {
			text = "SCORE: 2\nREASONING: weak"
		}
		results = append(results, scorer.BatchResult{Key: key, Text: text, PromptTokens: 100, CandidateTokens: 10, TotalTokens: 110})
	}
	api.statuses[first] = scorer.BatchStatus{Name: first, State: scorer.BatchStateSucceeded, Results: results}
	// The second batch stays running on the first poll, then expires.
	api.statuses[second] = scorer.BatchStatus{Name: second, State: scorer.BatchStateRunning}

	tenantFor := func(uid string) BatchTenant { return tenant }
	if _, err := PollScoreBatches(context.Background(), repo, api, tenantFor, cfg, discardLogger()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	// First batch closed; second still open.
	if n, err := repo.CountOpenScoreBatches(); err != nil || n != 1 {
		t.Fatalf("open after poll = (%d, %v), want 1", n, err)
	}

	// Scores landed per the guard: high scored+kept, low scored+archived,
	// raced dropped (user archive kept, no score).
	assertJob := func(id string, wantScore any, wantStatus string) {
		t.Helper()
		var score any
		var status, hash string
		if err := repo.RawDB().QueryRow(`SELECT score, status, COALESCE(resume_hash,'') FROM jobs WHERE id=?`, id).Scan(&score, &status, &hash); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if fmt.Sprint(score) != fmt.Sprint(wantScore) || status != wantStatus {
			t.Fatalf("%s = score %v status %s, want %v %s", id, score, status, wantScore, wantStatus)
		}
		if score != nil && hash != "hash-submit" {
			t.Fatalf("%s hash = %q, want submit-time hash", id, hash)
		}
	}
	for _, m := range []struct {
		id     string
		score  any
		status string
	}{
		{"p-high", 8, "pending"},
		{"p-low", 2, "archived"},
		{"p-raced", nil, "archived"},
	} {
		if memberOf[m.id] == first {
			assertJob(m.id, m.score, m.status)
		}
	}

	// Ledger recorded under the batch model key, not the sync model.
	if n, err := repo.DailyAPIUsage(scorer.BatchModel); err != nil || n == 0 {
		t.Fatalf("batch model usage = (%d, %v), want > 0", n, err)
	}
	if n, err := repo.DailyAPIUsage(scorer.Model); err != nil || n != 0 {
		t.Fatalf("sync model usage = (%d, %v), want 0", n, err)
	}

	// Expire the second batch: its jobs return to the pool unscored.
	api.statuses[second] = scorer.BatchStatus{Name: second, State: scorer.BatchStateExpired, Err: "expired"}
	if _, err := PollScoreBatches(context.Background(), repo, api, tenantFor, cfg, discardLogger()); err != nil {
		t.Fatalf("poll expired: %v", err)
	}
	if n, err := repo.CountOpenScoreBatches(); err != nil || n != 0 {
		t.Fatalf("open after expiry = (%d, %v), want 0", n, err)
	}
	unscored, err := repo.GetUnscoredJobs(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range unscored {
		if memberOf[j.ID] != second {
			t.Fatalf("unexpected unscored %s", j.ID)
		}
	}
	if len(unscored) == 0 {
		t.Fatal("expired batch's unscored members did not return to the pool")
	}
}

// backdateBatch ages a batch row past the exclusion bound, as if it had been
// wedged open for days.
func backdateBatch(t *testing.T, repo *db.Repository, name string) string {
	t.Helper()
	old := time.Now().UTC().Add(-db.OpenBatchExclusionMax - time.Hour).Format("2006-01-02 15:04:05")
	if _, err := repo.RawDB().Exec(`UPDATE score_batches SET created_at = ? WHERE batch_name = ?`, old, name); err != nil {
		t.Fatalf("backdate %s: %v", name, err)
	}
	return old
}

func batchState(t *testing.T, repo *db.Repository, name string) (state, errMsg string) {
	t.Helper()
	if err := repo.RawDB().QueryRow(`SELECT state, COALESCE(error,'') FROM score_batches WHERE batch_name = ?`, name).Scan(&state, &errMsg); err != nil {
		t.Fatalf("read batch %s: %v", name, err)
	}
	return state, errMsg
}

// A batch open past the exclusion bound is a wedge, not work in flight: the
// poll pass must evict it (close as expired, freeing its jobs) whether the
// poll reports it still running or fails outright. A fresh batch with a
// failing poll stays open, and a stale batch whose one last poll comes back
// SUCCEEDED is applied, not discarded.
func TestPollScoreBatchesEvictsStale(t *testing.T) {
	cfg := BatchScoreConfig{MaxOpenBatches: 4, DailyJobLimit: 100, MaxJobsPerSubmit: 10, ArchiveThreshold: 3}

	submitOne := func(t *testing.T, repo *db.Repository, api *fakeBatchAPI, tenant BatchTenant) string {
		t.Helper()
		if _, err := SubmitScoreBatches(context.Background(), repo, api, []BatchTenant{tenant}, cfg, discardLogger()); err != nil {
			t.Fatal(err)
		}
		return api.submits[len(api.submits)-1].name
	}
	poll := func(t *testing.T, repo *db.Repository, api *fakeBatchAPI, tenant BatchTenant) int {
		t.Helper()
		n, err := PollScoreBatches(context.Background(), repo, api, func(string) BatchTenant { return tenant }, cfg, discardLogger())
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		return n
	}

	t.Run("stale and still running upstream", func(t *testing.T) {
		repo, _ := newRepo(t)
		tenant := batchTestTenant(repo)
		api := &fakeBatchAPI{statuses: map[string]scorer.BatchStatus{}}
		seedUnscored(t, repo, "e1", "DevOps", "desc")
		name := submitOne(t, repo, api, tenant)
		backdateBatch(t, repo, name)

		poll(t, repo, api, tenant)
		if state, errMsg := batchState(t, repo, name); state != db.ScoreBatchExpired || !strings.Contains(errMsg, "evicted") {
			t.Fatalf("batch = (%s, %q), want evicted as expired", state, errMsg)
		}
		unscored, err := repo.GetUnscoredJobs(0)
		if err != nil || len(unscored) != 1 || unscored[0].ID != "e1" {
			t.Fatalf("member not freed: (%+v, %v)", unscored, err)
		}
	})

	t.Run("stale and poll fails", func(t *testing.T) {
		repo, _ := newRepo(t)
		tenant := batchTestTenant(repo)
		api := &fakeBatchAPI{statuses: map[string]scorer.BatchStatus{}, pollErrs: map[string]error{}}
		seedUnscored(t, repo, "e2", "SRE", "desc")
		name := submitOne(t, repo, api, tenant)
		backdateBatch(t, repo, name)
		api.pollErrs[name] = errors.New("401 key revoked")

		poll(t, repo, api, tenant)
		if state, errMsg := batchState(t, repo, name); state != db.ScoreBatchExpired || !strings.Contains(errMsg, "401 key revoked") {
			t.Fatalf("batch = (%s, %q), want evicted with the poll error", state, errMsg)
		}
	})

	t.Run("fresh and poll fails stays open", func(t *testing.T) {
		repo, _ := newRepo(t)
		tenant := batchTestTenant(repo)
		api := &fakeBatchAPI{statuses: map[string]scorer.BatchStatus{}, pollErrs: map[string]error{}}
		seedUnscored(t, repo, "e3", "Platform", "desc")
		name := submitOne(t, repo, api, tenant)
		api.pollErrs[name] = errors.New("transient 503")

		poll(t, repo, api, tenant)
		if state, _ := batchState(t, repo, name); state != db.ScoreBatchSubmitted {
			t.Fatalf("fresh batch = %s, want still submitted", state)
		}
	})

	t.Run("stale but succeeded applies", func(t *testing.T) {
		repo, _ := newRepo(t)
		tenant := batchTestTenant(repo)
		api := &fakeBatchAPI{statuses: map[string]scorer.BatchStatus{}}
		seedUnscored(t, repo, "e4", "Infra", "desc")
		name := submitOne(t, repo, api, tenant)
		backdateBatch(t, repo, name)
		api.statuses[name] = scorer.BatchStatus{Name: name, State: scorer.BatchStateSucceeded, Results: []scorer.BatchResult{
			{Key: "e4", Text: "SCORE: 8\nREASONING: strong", PromptTokens: 90, CandidateTokens: 10, TotalTokens: 100},
		}}

		if n := poll(t, repo, api, tenant); n != 1 {
			t.Fatalf("applied = %d, want 1", n)
		}
		if state, _ := batchState(t, repo, name); state != db.ScoreBatchSucceeded {
			t.Fatalf("batch = %s, want succeeded", state)
		}
	})
}

// One finished batch with mixed outcomes: a clean apply, an unparsable
// response (stamps score_error, job stays pending for the sync retry
// ordering), and a per-item API error. The batch closes, and the aggregated
// ledger counts every item that consumed model work exactly once.
func TestPollScoreBatchesMixedResults(t *testing.T) {
	repo, _ := newRepo(t)
	seedUnscored(t, repo, "m-ok", "DevOps", "desc")
	seedUnscored(t, repo, "m-bad", "SRE", "desc")
	seedUnscored(t, repo, "m-err", "Platform", "desc")

	api := &fakeBatchAPI{statuses: map[string]scorer.BatchStatus{}}
	cfg := BatchScoreConfig{MaxOpenBatches: 4, DailyJobLimit: 100, MaxJobsPerSubmit: 10, ArchiveThreshold: 3}
	tenant := batchTestTenant(repo)
	if _, err := SubmitScoreBatches(context.Background(), repo, api, []BatchTenant{tenant}, cfg, discardLogger()); err != nil {
		t.Fatal(err)
	}
	name := api.submits[0].name
	api.statuses[name] = scorer.BatchStatus{Name: name, State: scorer.BatchStateSucceeded, Results: []scorer.BatchResult{
		{Key: "m-ok", Text: "SCORE: 8\nREASONING: strong", PromptTokens: 90, CandidateTokens: 10, TotalTokens: 100},
		{Key: "m-bad", Text: "garbage the parser rejects", PromptTokens: 90, CandidateTokens: 10, TotalTokens: 100},
		{Key: "m-err", Err: "item quota exceeded"},
	}}

	n, err := PollScoreBatches(context.Background(), repo, api, func(string) BatchTenant { return tenant }, cfg, discardLogger())
	if err != nil || n != 1 {
		t.Fatalf("poll = (%d, %v), want (1, nil)", n, err)
	}
	if open, err := repo.CountOpenScoreBatches(); err != nil || open != 0 {
		t.Fatalf("open after poll = (%d, %v), want 0", open, err)
	}

	var score any
	var status string
	if err := repo.RawDB().QueryRow(`SELECT score, status FROM jobs WHERE id='m-ok'`).Scan(&score, &status); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(score) != "8" || status != "pending" {
		t.Fatalf("m-ok = (%v, %s), want (8, pending)", score, status)
	}
	for id, want := range map[string]string{"m-bad": "parse failed", "m-err": "item quota exceeded"} {
		var se string
		if err := repo.RawDB().QueryRow(`SELECT score, COALESCE(score_error,'') FROM jobs WHERE id=?`, id).Scan(&score, &se); err != nil {
			t.Fatal(err)
		}
		if score != nil || !strings.Contains(se, want) {
			t.Fatalf("%s = (score %v, error %q), want unscored with %q", id, score, se, want)
		}
	}

	// m-ok and m-bad consumed model work; m-err (no tokens, no text) did not.
	if calls, err := repo.DailyAPIUsage(scorer.BatchModel); err != nil || calls != 2 {
		t.Fatalf("ledger calls = (%d, %v), want 2", calls, err)
	}
	tok, err := repo.DailyAPITokens(scorer.BatchModel)
	if err != nil || tok.Total != 200 || tok.Prompt != 180 {
		t.Fatalf("ledger tokens = (%+v, %v), want total 200 prompt 180", tok, err)
	}
}
