package dashboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"job-search-automation/internal/db"
	"job-search-automation/internal/model"
	"job-search-automation/internal/scorer"
)

func seedLead(t *testing.T, repo *db.Repository, id, title, company string) {
	t.Helper()
	lead := model.Lead{
		JobLead: model.JobLead{
			Title: title, Company: company, Description: "desc",
			DirectApplyURL: "https://example.com/" + id, ATSPlatformName: "Greenhouse",
			ScrapedTimestamp: "2026-06-07T00:00:00.000Z", Location: "Remote", PostedAt: "2026-06-06",
		},
		ID: id,
	}
	if _, err := repo.InsertScrapedLead(lead); err != nil {
		t.Fatalf("seed lead %s: %v", id, err)
	}
}

// newRoutedServer starts a front door over a real repo at a known path, returning
// the test server, the repo, and a read-only verify connection.
func newRoutedServer(t *testing.T, rej RejectionScorer) (*httptest.Server, *db.Repository, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jobs.db")
	repo, err := db.Open(path)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	srv, err := New(t.TempDir(), repo, rej, 5*time.Second, 500, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	verify, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("verify open: %v", err)
	}
	t.Cleanup(func() { _ = verify.Close() })
	return ts, repo, verify
}

func statusOf(t *testing.T, verify *sql.DB, id string) string {
	t.Helper()
	var s string
	if err := verify.QueryRow("SELECT status FROM jobs WHERE id=?", id).Scan(&s); err != nil {
		t.Fatalf("status %s: %v", id, err)
	}
	return s
}

func TestScraperHeartbeatRoute(t *testing.T) {
	ts, repo, _ := newRoutedServer(t, nil)

	// Absent heartbeat -> null.
	resp := get(t, ts.URL+"/api/scraper-heartbeat", nil)
	var body struct {
		Heartbeat json.RawMessage `json:"heartbeat"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if string(body.Heartbeat) != "null" {
		t.Fatalf("absent heartbeat = %s, want null", body.Heartbeat)
	}

	// Present heartbeat -> the stored object.
	if err := repo.WriteHeartbeat("ok", 3, 1, ""); err != nil {
		t.Fatalf("write hb: %v", err)
	}
	resp2 := get(t, ts.URL+"/api/scraper-heartbeat", nil)
	defer resp2.Body.Close()
	raw, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(raw), `"status":"ok"`) {
		t.Fatalf("heartbeat route body = %s", raw)
	}
}

func TestScoringProgressRoute(t *testing.T) {
	ts, repo, _ := newRoutedServer(t, nil)
	seedLead(t, repo, "u1", "Eng", "Co")
	seedLead(t, repo, "u2", "Eng", "Co")

	resp := get(t, ts.URL+"/api/scoring-progress", nil)
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["unscored"].(float64) != 2 {
		t.Fatalf("unscored = %v, want 2", body["unscored"])
	}
	if body["total"].(float64) != 2 {
		t.Fatalf("total = %v, want 2", body["total"])
	}
	// 2 unscored * 5s rate delay = 10s ETA.
	if body["etaSeconds"].(float64) != 10 {
		t.Fatalf("etaSeconds = %v, want 10", body["etaSeconds"])
	}
	if body["latestScoreAt"] != nil {
		t.Fatalf("latestScoreAt = %v, want null", body["latestScoreAt"])
	}
	if body["lastScrapeAt"] != nil {
		t.Fatalf("lastScrapeAt = %v, want null with no heartbeat", body["lastScrapeAt"])
	}

	if err := repo.WriteHeartbeat("ok", 9, 4, ""); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	resp2 := get(t, ts.URL+"/api/scoring-progress", nil)
	defer resp2.Body.Close()
	body = map[string]any{}
	if err := json.NewDecoder(resp2.Body).Decode(&body); err != nil {
		t.Fatalf("decode 2: %v", err)
	}
	if body["lastScrapeAt"] == nil || body["lastScrapeStatus"] != "ok" {
		t.Fatalf("heartbeat fields = %v / %v", body["lastScrapeAt"], body["lastScrapeStatus"])
	}
	if body["lastScrapeInserted"].(float64) != 4 {
		t.Fatalf("lastScrapeInserted = %v, want 4", body["lastScrapeInserted"])
	}
}

// With a scrape schedule configured, the progress payload reports the next run
// time so the browser can show "next scrape <time>". Without one the field is
// omitted (default test server), as asserted implicitly above.
func TestScoringProgressNextScrapeAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	repo, err := db.Open(path)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	srv, err := New(t.TempDir(), repo, nil, 5*time.Second, 500, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.SetScrapeSchedule("0 */6 * * *")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp := get(t, ts.URL+"/api/scoring-progress", nil)
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	next, ok := body["nextScrapeAt"].(string)
	if !ok || next == "" {
		t.Fatalf("nextScrapeAt = %v, want an RFC3339 time", body["nextScrapeAt"])
	}
	tt, perr := time.Parse(time.RFC3339, next)
	if perr != nil {
		t.Fatalf("nextScrapeAt not RFC3339: %v", perr)
	}
	if !tt.After(time.Now()) {
		t.Fatalf("nextScrapeAt = %v, want a future time", tt)
	}
}

// On self-host the request tenant is LocalUser, so DELETE /api/account must
// refuse rather than wipe the shared single-tenant data. (The hosted success
// path delegates to repo.DeleteTenant, covered in internal/db.)
func TestDeleteAccountSelfHostRefused(t *testing.T) {
	ts, repo, verify := newRoutedServer(t, nil)
	seedLead(t, repo, "keep1", "Eng", "Co")

	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/api/account", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete account: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	var n int
	if err := verify.QueryRow("SELECT COUNT(*) FROM jobs").Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("jobs after refused delete = %d, want 1 (nothing wiped)", n)
	}
}

func TestJobDescriptionRoute(t *testing.T) {
	ts, repo, _ := newRoutedServer(t, nil)
	seedLead(t, repo, "j", "Backend Engineer", "Acme")

	resp := get(t, ts.URL+"/job-description?id=j", nil)
	defer resp.Body.Close()
	var d db.JobDescription
	_ = json.NewDecoder(resp.Body).Decode(&d)
	if d.Title != "Backend Engineer" || d.Company != "Acme" {
		t.Fatalf("desc = %+v", d)
	}

	if r := get(t, ts.URL+"/job-description?id=nope", nil); r.StatusCode != http.StatusNotFound {
		r.Body.Close()
		t.Fatalf("missing id status = %d, want 404", r.StatusCode)
	}
	if r := get(t, ts.URL+"/job-description", nil); r.StatusCode != http.StatusBadRequest {
		r.Body.Close()
		t.Fatalf("no id status = %d, want 400", r.StatusCode)
	}
}

func TestCompanyNotesRoutes(t *testing.T) {
	ts, _, _ := newRoutedServer(t, nil)

	// Save (tags get normalized/sorted/deduped).
	postJSON(t, ts.URL+"/company-notes", `{"company":"  Acme  ","tags":"YC, Remote, yc","notes":"hi"}`).Body.Close()

	resp := get(t, ts.URL+"/company-notes?company=acme", nil)
	defer resp.Body.Close()
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["tags"] != "remote, yc" {
		t.Fatalf("tags = %q, want 'remote, yc'", body["tags"])
	}
	if body["notes"] != "hi" {
		t.Fatalf("notes = %q", body["notes"])
	}

	// Unknown company -> empty payload, not 404.
	r := get(t, ts.URL+"/company-notes?company=ghost", nil)
	defer r.Body.Close()
	var empty map[string]string
	_ = json.NewDecoder(r.Body).Decode(&empty)
	if empty["tags"] != "" || empty["notes"] != "" {
		t.Fatalf("unknown company = %v", empty)
	}
}

func TestArchiveRoute(t *testing.T) {
	ts, repo, verify := newRoutedServer(t, nil)
	seedLead(t, repo, "a", "Eng", "Co")

	resp := postJSON(t, ts.URL+"/archive", `{"id":"a"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("archive status = %d", resp.StatusCode)
	}
	if got := statusOf(t, verify, "a"); got != "archived" {
		t.Fatalf("status = %q, want archived", got)
	}
}

func TestPipelineRouteValidatesAndTransitions(t *testing.T) {
	rej := &fakeRejection{}
	ts, repo, verify := newRoutedServer(t, rej)
	seedLead(t, repo, "p", "Eng", "Co")

	// Bad value -> 400 with the standard error envelope ({ok:false,error}).
	if r := postJSON(t, ts.URL+"/pipeline", `{"id":"p","value":"banana"}`); r.StatusCode != http.StatusBadRequest {
		r.Body.Close()
		t.Fatalf("bad value status = %d, want 400", r.StatusCode)
	} else {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if got := strings.TrimSpace(string(body)); got != `{"ok":false,"error":"bad pipeline value"}` {
			t.Fatalf("bad value body = %q, want standard error envelope", got)
		}
	}

	// Apply -> 200, status applied, and the rejection hook fires.
	r := postJSON(t, ts.URL+"/pipeline", `{"id":"p","value":"applied"}`)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("apply status = %d", r.StatusCode)
	}
	if got := statusOf(t, verify, "p"); got != "applied" {
		t.Fatalf("status = %q, want applied", got)
	}

	// The background rejection scorer should run and store its text.
	waitFor(t, func() bool { return rej.calls.Load() >= 1 })
	waitFor(t, func() bool {
		var rr string
		_ = verify.QueryRow("SELECT COALESCE(rejection_reasoning,'') FROM jobs WHERE id='p'").Scan(&rr)
		return rr == "likely rejected: stack"
	})
}

// --- helpers ---

type fakeRejection struct {
	calls atomic.Int32
	askFn func(prompt string) (string, error)
}

func (f *fakeRejection) ScoreRejection(_ context.Context, _ scorer.Job) (string, error) {
	f.calls.Add(1)
	return "likely rejected: stack", nil
}

func (f *fakeRejection) Ask(_ context.Context, prompt string, _ int) (string, error) {
	if f.askFn != nil {
		return f.askFn(prompt)
	}
	return "an answer", nil
}

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}
