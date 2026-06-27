package db

import (
	"database/sql"
	"strings"
	"testing"
)

func TestScraperHeartbeatRaw(t *testing.T) {
	repo := newTestRepo(t)

	raw, err := repo.ScraperHeartbeatRaw()
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if raw != "" {
		t.Fatalf("expected empty heartbeat, got %q", raw)
	}

	if err := repo.WriteHeartbeat("ok", 5, 2, ""); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, err = repo.ScraperHeartbeatRaw()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if raw == "" || !strings.Contains(raw, `"status":"ok"`) {
		t.Fatalf("heartbeat raw = %q", raw)
	}
}

func TestScoringStats(t *testing.T) {
	repo := newTestRepo(t)
	seedJob(t, repo, "u1", "Eng", "Co")
	seedJob(t, repo, "u2", "Eng", "Co")
	seedJob(t, repo, "s1", "Eng", "Co")
	if err := repo.UpdateJobScore("s1", 8, "good"); err != nil {
		t.Fatalf("score: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := repo.RecordAPICall("gemini-3.1-flash-lite"); err != nil {
			t.Fatalf("usage: %v", err)
		}
	}

	st, err := repo.ScoringStats()
	if err != nil {
		t.Fatalf("ScoringStats: %v", err)
	}
	if st.Scored != 1 {
		t.Fatalf("scored = %d, want 1", st.Scored)
	}
	if st.Unscored != 2 {
		t.Fatalf("unscored = %d, want 2", st.Unscored)
	}
	if st.APIUsedToday != 3 {
		t.Fatalf("apiUsedToday = %d, want 3", st.APIUsedToday)
	}
	if st.NewJobs24h < 3 {
		t.Fatalf("newJobs24h = %d, want >= 3", st.NewJobs24h)
	}
	if st.StrongFitsNew24h != 1 {
		t.Fatalf("strongFitsNew24h = %d, want 1 (s1 scored 8)", st.StrongFitsNew24h)
	}
	if st.LatestScoreAt == "" {
		t.Fatal("latestScoreAt should be set after scoring")
	}
}

func TestDiscoveryReport(t *testing.T) {
	repo := newTestRepo(t)

	if _, found, err := repo.DiscoveryReport(); err != nil || found {
		t.Fatalf("empty: found=%v err=%v", found, err)
	}
	if err := repo.WriteDiscoveryReport(3); err != nil {
		t.Fatalf("write: %v", err)
	}
	m, found, err := repo.DiscoveryReport()
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	if m.Added != 3 || m.At == "" {
		t.Fatalf("report = %+v", m)
	}
}

func TestJobDescription(t *testing.T) {
	repo := newTestRepo(t)
	seedJob(t, repo, "j", "Backend Engineer", "Acme")

	d, found, err := repo.JobDescription("j")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if d.Title != "Backend Engineer" || d.Company != "Acme" {
		t.Fatalf("unexpected desc: %+v", d)
	}

	_, found, err = repo.JobDescription("missing")
	if err != nil {
		t.Fatalf("missing err: %v", err)
	}
	if found {
		t.Fatal("missing job should report found=false")
	}
}

func TestCompanyNotesRoundTrip(t *testing.T) {
	repo := newTestRepo(t)

	_, _, found, err := repo.CompanyNotes("acme")
	if err != nil || found {
		t.Fatalf("expected not found, got found=%v err=%v", found, err)
	}

	if err := repo.SaveCompanyNotes("acme", "remote, yc", "great team"); err != nil {
		t.Fatalf("save: %v", err)
	}
	tags, notes, found, err := repo.CompanyNotes("acme")
	if err != nil || !found {
		t.Fatalf("read found=%v err=%v", found, err)
	}
	if tags != "remote, yc" || notes != "great team" {
		t.Fatalf("got tags=%q notes=%q", tags, notes)
	}

	// Upsert overwrites.
	if err := repo.SaveCompanyNotes("acme", "remote", "updated"); err != nil {
		t.Fatalf("update: %v", err)
	}
	tags, notes, _, _ = repo.CompanyNotes("acme")
	if tags != "remote" || notes != "updated" {
		t.Fatalf("after update got tags=%q notes=%q", tags, notes)
	}
}

func TestArchiveJobLogsEvent(t *testing.T) {
	repo := newTestRepo(t)
	seedJob(t, repo, "a", "Eng", "Co")

	if err := repo.ArchiveJob("a"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if got := status(t, repo, "a"); got != "archived" {
		t.Fatalf("status = %q, want archived", got)
	}
	var n int
	if err := repo.db.QueryRow(
		"SELECT COUNT(*) FROM events WHERE job_id='a' AND event_type='status_change' AND to_value='archived'",
	).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 1 {
		t.Fatalf("archive events = %d, want 1", n)
	}
}

func TestSetPipelineStageBranches(t *testing.T) {
	repo := newTestRepo(t)

	read := func(id string) (status, stage, rejectedFrom string, appliedSet bool) {
		t.Helper()
		var st, sg, rf, ap sql.NullString
		if err := repo.db.QueryRow(
			"SELECT status, stage, rejected_from_stage, applied_at FROM jobs WHERE id=?", id,
		).Scan(&st, &sg, &rf, &ap); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return st.String, sg.String, rf.String, ap.Valid
	}

	// applied (a real stage value).
	seedJob(t, repo, "ap", "Eng", "Co")
	if err := repo.SetPipelineStage("ap", "phone_screen"); err != nil {
		t.Fatalf("applied: %v", err)
	}
	if st, sg, _, appliedSet := read("ap"); st != "applied" || sg != "phone_screen" || !appliedSet {
		t.Fatalf("applied branch: status=%q stage=%q appliedSet=%v", st, sg, appliedSet)
	}

	// rejected carries the prior stage into rejected_from_stage.
	if err := repo.SetPipelineStage("ap", "rejected"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if st, sg, rf, _ := read("ap"); st != "rejected" || sg != "rejected" || rf != "phone_screen" {
		t.Fatalf("rejected branch: status=%q stage=%q rejectedFrom=%q", st, sg, rf)
	}

	// closed.
	seedJob(t, repo, "cl", "Eng", "Co")
	if err := repo.SetPipelineStage("cl", "closed"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if st, sg, _, _ := read("cl"); st != "closed" || sg != "closed" {
		t.Fatalf("closed branch: status=%q stage=%q", st, sg)
	}

	// clear (empty value) resets to pending with NULL stage.
	seedJob(t, repo, "cr", "Eng", "Co")
	if err := repo.SetPipelineStage("cr", "interview"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := repo.SetPipelineStage("cr", ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if st, sg, _, _ := read("cr"); st != "pending" || sg != "" {
		t.Fatalf("clear branch: status=%q stage=%q (stage should be NULL)", st, sg)
	}

	// Each transition logs a stage_change event.
	var n int
	if err := repo.db.QueryRow(
		"SELECT COUNT(*) FROM events WHERE event_type='stage_change'",
	).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n < 5 {
		t.Fatalf("stage_change events = %d, want >= 5", n)
	}
}

func TestJobBriefAndRejectionReasoning(t *testing.T) {
	repo := newTestRepo(t)
	seedJob(t, repo, "j", "SRE", "Globex")

	brief, found, err := repo.JobBrief("j")
	if err != nil || !found {
		t.Fatalf("brief found=%v err=%v", found, err)
	}
	if brief.Title != "SRE" || brief.Company != "Globex" || brief.Location != "Remote" {
		t.Fatalf("brief = %+v", brief)
	}

	if err := repo.SetRejectionReasoning("j", "stack mismatch"); err != nil {
		t.Fatalf("set reasoning: %v", err)
	}
	var got string
	if err := repo.db.QueryRow("SELECT rejection_reasoning FROM jobs WHERE id='j'").Scan(&got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "stack mismatch" {
		t.Fatalf("rejection_reasoning = %q", got)
	}
}
