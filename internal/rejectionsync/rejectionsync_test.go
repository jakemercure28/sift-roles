package rejectionsync

import (
	"context"
	"path/filepath"
	"testing"

	"job-search-automation/internal/db"
)

type fakeFetcher struct {
	result FetchResult
	err    error
}

func (f fakeFetcher) FetchMailboxMessages(context.Context, FetchOptions) (FetchResult, error) {
	return f.result, f.err
}

func newTestDB(t *testing.T) *db.Repository {
	t.Helper()
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func seedAppliedJob(t *testing.T, repo *db.Repository, id, company, title string) {
	t.Helper()
	_, err := repo.RawDB().Exec(`
		INSERT INTO jobs (
			id, company, title, url, platform, status, stage, applied_at
		) VALUES (?, ?, ?, ?, 'greenhouse', 'applied', 'phone_screen', '2026-06-01T12:00:00Z')
	`, id, company, title, "https://boards.greenhouse.io/"+company+"/jobs/123")
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
}

func TestIsRejectionEmail(t *testing.T) {
	message := Message{
		Subject:     "Your application",
		FromAddress: "recruiting@example.com",
		Raw:         "Thank you for applying. Unfortunately, we will not be moving forward.",
	}
	if !IsRejectionEmail(message) {
		t.Fatal("expected rejection email")
	}

	neutral := Message{Subject: "Next steps", Raw: "We would like to schedule another interview."}
	if IsRejectionEmail(neutral) {
		t.Fatal("did not expect neutral email to classify as rejection")
	}
}

func TestSyncAppliesSingleCompanyRejection(t *testing.T) {
	repo := newTestDB(t)
	seedAppliedJob(t, repo, "job-1", "Acme", "Senior SRE")

	message := Message{
		UID:         11,
		Subject:     "Acme application update",
		FromAddress: "recruiting@acme.test",
		MessageID:   "<msg-11@acme.test>",
		ReceivedAt:  "2026-06-08T15:04:05Z",
		Raw:         "Hi, thanks for your interest in Acme. Unfortunately, we decided not to move forward.",
	}

	summary, err := Sync(context.Background(), repo, Config{
		SkipTrash: true,
		Fetcher: fakeFetcher{result: FetchResult{
			UIDValidity: "777",
			LastUID:     uint32Ptr(11),
			Messages:    []Message{message},
		}},
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if summary != (Summary{Fetched: 1, Candidates: 1, Applied: 1}) {
		t.Fatalf("summary = %+v", summary)
	}

	var status, stage, rejectedFrom, rejectedAt string
	if err := repo.RawDB().QueryRow(`
		SELECT status, stage, rejected_from_stage, rejected_at
		FROM jobs WHERE id = 'job-1'
	`).Scan(&status, &stage, &rejectedFrom, &rejectedAt); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if status != "rejected" || stage != "rejected" || rejectedFrom != "phone_screen" || rejectedAt != message.ReceivedAt {
		t.Fatalf("job rejection state = status=%q stage=%q from=%q at=%q", status, stage, rejectedFrom, rejectedAt)
	}

	var eventCount int
	if err := repo.RawDB().QueryRow(`
		SELECT COUNT(*) FROM events
		WHERE job_id = 'job-1' AND event_type = 'stage_change'
			AND from_value = 'phone_screen' AND to_value = 'rejected'
	`).Scan(&eventCount); err != nil {
		t.Fatalf("read events: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("event count = %d, want 1", eventCount)
	}

	var matchStatus, reason, matchedID string
	if err := repo.RawDB().QueryRow(`
		SELECT match_status, reason, matched_job_id
		FROM rejection_email_log
		WHERE mailbox = ? AND uid_validity = '777' AND uid = 11
	`, DefaultMailbox).Scan(&matchStatus, &reason, &matchedID); err != nil {
		t.Fatalf("read rejection log: %v", err)
	}
	if matchStatus != "applied" || reason != "single_active_company_job" || matchedID != "job-1" {
		t.Fatalf("log row = status=%q reason=%q matched=%q", matchStatus, reason, matchedID)
	}
}

func TestSyncLeavesAmbiguousCompanyUnmatched(t *testing.T) {
	repo := newTestDB(t)
	seedAppliedJob(t, repo, "job-1", "Acme", "Senior SRE")
	seedAppliedJob(t, repo, "job-2", "Acme", "Platform Engineer")

	summary, err := Sync(context.Background(), repo, Config{
		SkipTrash: true,
		Fetcher: fakeFetcher{result: FetchResult{
			UIDValidity: "777",
			LastUID:     uint32Ptr(12),
			Messages: []Message{{
				UID:         12,
				Subject:     "Acme application update",
				FromAddress: "recruiting@acme.test",
				Raw:         "Unfortunately, Acme decided not to proceed with your candidacy.",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if summary != (Summary{Fetched: 1, Candidates: 1, Unmatched: 1}) {
		t.Fatalf("summary = %+v", summary)
	}
	var reason string
	if err := repo.RawDB().QueryRow(`
		SELECT reason FROM rejection_email_log
		WHERE mailbox = ? AND uid_validity = '777' AND uid = 12
	`, DefaultMailbox).Scan(&reason); err != nil {
		t.Fatalf("read rejection log: %v", err)
	}
	if reason != "ambiguous_company_match" {
		t.Fatalf("reason = %q, want ambiguous_company_match", reason)
	}
}

func uint32Ptr(v uint32) *uint32 { return &v }
