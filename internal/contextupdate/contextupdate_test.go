package contextupdate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"job-search-automation/internal/db"
)

func TestRunWritesApplicationAndCareerContext(t *testing.T) {
	repo, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer repo.Close()

	_, err = repo.RawDB().Exec(`
		INSERT INTO jobs (
			id, company, title, url, platform, status, stage, score, applied_at,
			posted_at, rejected_at, rejected_from_stage
		) VALUES
			('job-interview', 'Acme', 'Senior SRE', 'https://example.com/1', 'greenhouse',
				'applied', 'phone_screen', 9, '2026-06-04T12:00:00Z', '2026-06-01', NULL, NULL),
			('job-rejected', 'Beta', 'Staff Platform Engineer With A Very Long Title', 'https://example.com/2', 'ashby',
				'rejected', 'rejected', 7, '2026-06-02T12:00:00Z', '2026-05-31', '2026-06-08T12:00:00Z', 'interview')
	`)
	if err != nil {
		t.Fatalf("seed jobs: %v", err)
	}

	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "goals", "career.md"), []byte("# Career\n\nManual notes.\n"), 0o644); err == nil {
		t.Fatal("expected write before mkdir to fail")
	}
	if err := os.MkdirAll(filepath.Join(contextDir, "goals"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextDir, "goals", "career.md"), []byte("# Career\n\nManual notes.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	summary, err := Run(context.Background(), repo, Config{
		ContextDir: contextDir,
		RepoRoot:   contextDir,
		Now:        time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if summary.ActiveInterviews != 1 || summary.Rejections != 1 || !summary.ApplicationsUpdated || !summary.CareerUpdated {
		t.Fatalf("summary = %+v", summary)
	}

	applications := readFile(t, filepath.Join(contextDir, "reference", "applications.md"))
	for _, want := range []string{
		"Auto-updated by context-update. Last run: 2026-06-09",
		"### Acme / Senior SRE",
		"| Beta | Staff Platform Engineer With A Very Long | 7 | interview | 6.0d | 2.5d |",
		"**Averages:** 6.0 days to rejection, 2.5 days posting age at application",
	} {
		if !strings.Contains(applications, want) {
			t.Fatalf("applications.md missing %q:\n%s", want, applications)
		}
	}

	career := readFile(t, filepath.Join(contextDir, "goals", "career.md"))
	for _, want := range []string{
		"# Career",
		"Manual notes.",
		"<!-- AUTO-STATS-START -->",
		"- **Applied:** 1",
		"- **Interviewing:** 1 (100% conversion from applied)",
		"- **Rejected:** 1",
	} {
		if !strings.Contains(career, want) {
			t.Fatalf("career.md missing %q:\n%s", want, career)
		}
	}
}

func TestReplaceMarkedBlock(t *testing.T) {
	got := replaceMarkedBlock("before\n<!-- A -->\nold\n<!-- B -->\nafter\n", "<!-- A -->", "<!-- B -->", "<!-- A -->\nnew\n<!-- B -->")
	want := "before\n<!-- A -->\nnew\n<!-- B -->\nafter\n"
	if got != want {
		t.Fatalf("replaceMarkedBlock = %q, want %q", got, want)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
