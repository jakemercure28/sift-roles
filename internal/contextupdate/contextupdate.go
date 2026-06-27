package contextupdate

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"job-search-automation/internal/db"
)

type Config struct {
	ContextDir string
	RepoRoot   string
	Now        time.Time
	Log        *slog.Logger
}

type Summary struct {
	ApplicationsUpdated bool `json:"applicationsUpdated"`
	CareerUpdated       bool `json:"careerUpdated"`
	ArchitectureUpdated bool `json:"architectureUpdated"`
	ActiveInterviews    int  `json:"activeInterviews"`
	Rejections          int  `json:"rejections"`
	RecentPRs           int  `json:"recentPRs"`
}

type rejectionRow struct {
	Company           string
	Title             string
	Score             sql.NullInt64
	AppliedAt         sql.NullString
	RejectedAt        sql.NullString
	RejectedFromStage sql.NullString
	DaysToReject      sql.NullFloat64
	PostingAge        sql.NullFloat64
}

type interviewRow struct {
	Company   string
	Title     string
	Score     sql.NullInt64
	Stage     string
	AppliedAt sql.NullString
}

type careerStats struct {
	Total        int
	Applied      int
	Interviewing int
	Rejected     int
	Offers       int
	WeekApps     int
}

func Run(ctx context.Context, repo *db.Repository, cfg Config) (Summary, error) {
	if cfg.ContextDir == "" {
		cfg.ContextDir = ".context"
	}
	if cfg.RepoRoot == "" {
		cfg.RepoRoot = "."
	}
	if cfg.Now.IsZero() {
		cfg.Now = time.Now()
	}

	if err := os.MkdirAll(filepath.Join(cfg.ContextDir, "reference"), 0o755); err != nil {
		return Summary{}, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.ContextDir, "goals"), 0o755); err != nil {
		return Summary{}, err
	}
	if err := os.MkdirAll(filepath.Join(cfg.ContextDir, "decisions"), 0o755); err != nil {
		return Summary{}, err
	}

	var summary Summary
	appUpdated, active, rejected, err := updateApplications(ctx, repo, cfg)
	if err != nil {
		return Summary{}, err
	}
	summary.ApplicationsUpdated = appUpdated
	summary.ActiveInterviews = active
	summary.Rejections = rejected

	careerUpdated, err := updateCareerStats(ctx, repo, cfg)
	if err != nil {
		return Summary{}, err
	}
	summary.CareerUpdated = careerUpdated

	archUpdated, recent, err := updateRecentPRs(ctx, cfg)
	if err != nil {
		if cfg.Log != nil {
			cfg.Log.Warn("context architecture update skipped", "error", err)
		}
	} else {
		summary.ArchitectureUpdated = archUpdated
		summary.RecentPRs = recent
	}
	return summary, nil
}

func updateApplications(ctx context.Context, repo *db.Repository, cfg Config) (bool, int, int, error) {
	rejections, err := queryRejections(ctx, repo)
	if err != nil {
		return false, 0, 0, err
	}
	interviews, err := queryInterviews(ctx, repo)
	if err != nil {
		return false, 0, 0, err
	}

	var b strings.Builder
	date := cfg.Now.UTC().Format("2006-01-02")
	b.WriteString("# Application Notes\n\n")
	b.WriteString("Auto-updated by context-update. Last run: " + date + "\n\n")
	b.WriteString("## Active Interviews\n\n")

	if len(interviews) == 0 {
		b.WriteString("No active interviews.\n\n")
	} else {
		for _, j := range interviews {
			stage := stageLabel(j.Stage)
			b.WriteString("### " + j.Company + " / " + j.Title + "\n")
			b.WriteString(fmt.Sprintf("**Stage:** %s | **Score:** %s | **Applied:** %s\n\n",
				stage, intOrQuestion(j.Score), datePrefix(j.AppliedAt)))
		}
	}

	b.WriteString(fmt.Sprintf("## Rejections (%d total)\n\n", len(rejections)))
	if len(rejections) > 0 {
		b.WriteString("| Company | Role | Score | From | Days to Reject | Posting Age |\n")
		b.WriteString("|---------|------|-------|------|----------------|-------------|\n")
		for _, r := range rejections {
			from := r.RejectedFromStage.String
			if from == "" {
				from = "applied"
			}
			b.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s |\n",
				r.Company,
				truncateRunes(r.Title, 40),
				intOrQuestion(r.Score),
				from,
				floatDaysOrQuestion(r.DaysToReject),
				floatDaysOrQuestion(r.PostingAge),
			))
		}
		avgDays, hasDays := average(rejections, func(r rejectionRow) sql.NullFloat64 { return r.DaysToReject })
		avgAge, hasAge := average(rejections, func(r rejectionRow) sql.NullFloat64 { return r.PostingAge })
		staleRejects := 0
		for _, r := range rejections {
			if r.PostingAge.Valid && r.PostingAge.Float64 > 30 {
				staleRejects++
			}
		}
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("**Averages:** %s days to rejection, %s days posting age at application\n",
			avgOrQuestion(avgDays, hasDays), avgOrQuestion(avgAge, hasAge)))
		b.WriteString(fmt.Sprintf("**Stale postings (>30 days old at apply):** %d of %d rejections\n", staleRejects, len(rejections)))
	}

	changed, err := writeIfChanged(filepath.Join(cfg.ContextDir, "reference", "applications.md"), b.String())
	return changed, len(interviews), len(rejections), err
}

func queryRejections(ctx context.Context, repo *db.Repository) ([]rejectionRow, error) {
	rows, err := repo.RawDB().QueryContext(ctx, repo.Rewrite(`
		SELECT j.company, j.title, j.score, j.applied_at, j.rejected_at,
			j.rejected_from_stage,
			ROUND(julianday(j.rejected_at) - julianday(j.applied_at), 1) AS days_to_reject,
			ROUND(julianday(j.applied_at) - julianday(j.posted_at), 1) AS posting_age
		FROM jobs j
		WHERE j.stage = 'rejected' AND j.rejected_at IS NOT NULL AND j.user_id = ?
		ORDER BY j.rejected_at DESC
	`), repo.UserID())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rejectionRow
	for rows.Next() {
		var r rejectionRow
		if err := rows.Scan(&r.Company, &r.Title, &r.Score, &r.AppliedAt, &r.RejectedAt,
			&r.RejectedFromStage, &r.DaysToReject, &r.PostingAge); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func queryInterviews(ctx context.Context, repo *db.Repository) ([]interviewRow, error) {
	rows, err := repo.RawDB().QueryContext(ctx, repo.Rewrite(`
		SELECT company, title, score, stage, applied_at
		FROM jobs
		WHERE stage IN ('phone_screen', 'interview', 'onsite', 'offer') AND user_id = ?
		ORDER BY applied_at DESC
	`), repo.UserID())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []interviewRow
	for rows.Next() {
		var r interviewRow
		if err := rows.Scan(&r.Company, &r.Title, &r.Score, &r.Stage, &r.AppliedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func updateCareerStats(ctx context.Context, repo *db.Repository, cfg Config) (bool, error) {
	stats, err := queryCareerStats(ctx, repo)
	if err != nil {
		return false, err
	}
	convRate := 0
	if stats.Applied > 0 {
		convRate = int(float64(stats.Interviewing)/float64(stats.Applied)*100 + 0.5)
	}
	date := cfg.Now.UTC().Format("2006-01-02")
	statsBlock := fmt.Sprintf(`%s
## Pipeline Snapshot (auto-updated %s)

- **Total tracked:** %d
- **Applied:** %d
- **Interviewing:** %d (%d%% conversion from applied)
- **Rejected:** %d
- **Offers:** %d
- **Applied this week:** %d
%s`, markerStatsStart, date, stats.Total, stats.Applied, stats.Interviewing,
		convRate, stats.Rejected, stats.Offers, stats.WeekApps, markerStatsEnd)

	path := filepath.Join(cfg.ContextDir, "goals", "career.md")
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	content := replaceMarkedBlock(string(existing), markerStatsStart, markerStatsEnd, statsBlock)
	return writeIfChanged(path, content)
}

func queryCareerStats(ctx context.Context, repo *db.Repository) (careerStats, error) {
	// Every count is scoped to the tenant with a single trailing user_id bind, so
	// the repo.UserID() arg below applies uniformly.
	queries := []struct {
		dst *int
		sql string
	}{}
	var s careerStats
	queries = append(queries,
		struct {
			dst *int
			sql string
		}{&s.Total, "SELECT COUNT(*) FROM jobs WHERE status != 'archived' AND user_id = ?"},
		struct {
			dst *int
			sql string
		}{&s.Applied, "SELECT COUNT(*) FROM jobs WHERE status IN ('applied','responded') AND user_id = ?"},
		struct {
			dst *int
			sql string
		}{&s.Interviewing, "SELECT COUNT(*) FROM jobs WHERE stage IN ('phone_screen','interview','onsite','offer') AND user_id = ?"},
		struct {
			dst *int
			sql string
		}{&s.Rejected, "SELECT COUNT(*) FROM jobs WHERE stage = 'rejected' AND user_id = ?"},
		struct {
			dst *int
			sql string
		}{&s.Offers, "SELECT COUNT(*) FROM jobs WHERE stage = 'offer' AND user_id = ?"},
		struct {
			dst *int
			sql string
		}{&s.WeekApps, "SELECT COUNT(*) FROM jobs WHERE applied_at IS NOT NULL AND julianday('now') - julianday(applied_at) <= 7 AND user_id = ?"},
	)
	for _, q := range queries {
		if err := repo.RawDB().QueryRowContext(ctx, repo.Rewrite(q.sql), repo.UserID()).Scan(q.dst); err != nil {
			return careerStats{}, err
		}
	}
	return s, nil
}

func updateRecentPRs(ctx context.Context, cfg Config) (bool, int, error) {
	if _, err := os.Stat(filepath.Join(cfg.RepoRoot, ".git")); err != nil {
		return false, 0, nil
	}
	if _, err := exec.LookPath("git"); err != nil {
		return false, 0, nil
	}
	out, err := gitRecentMerges(ctx, cfg.RepoRoot)
	if err != nil || strings.TrimSpace(out) == "" {
		return false, 0, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) > 15 {
		lines = lines[:15]
	}
	date := cfg.Now.UTC().Format("2006-01-02")
	prBlock := markerPRStart + "\n" +
		"## Recent Changes (auto-updated " + date + ")\n\n" +
		"- " + strings.Join(lines, "\n- ") + "\n" +
		markerPREnd

	path := filepath.Join(cfg.ContextDir, "decisions", "architecture.md")
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, 0, err
	}
	content := replaceMarkedBlock(string(existing), markerPRStart, markerPREnd, prBlock)
	changed, err := writeIfChanged(path, content)
	return changed, len(lines), err
}

func gitRecentMerges(ctx context.Context, repoRoot string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "log", "--oneline", "--merges", "--since=7 days ago", "--grep=Merge pull request")
	cmd.Dir = repoRoot
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git log recent PRs: %s", msg)
	}
	return string(out), nil
}

const (
	markerStatsStart = "<!-- AUTO-STATS-START -->"
	markerStatsEnd   = "<!-- AUTO-STATS-END -->"
	markerPRStart    = "<!-- AUTO-RECENT-PRS-START -->"
	markerPREnd      = "<!-- AUTO-RECENT-PRS-END -->"
)

func replaceMarkedBlock(existing, start, end, block string) string {
	if strings.Contains(existing, start) && strings.Contains(existing, end) {
		before := existing[:strings.Index(existing, start)]
		afterStart := strings.Index(existing, end) + len(end)
		after := existing[afterStart:]
		return before + block + after
	}
	if strings.TrimSpace(existing) == "" {
		return block + "\n"
	}
	return strings.TrimRight(existing, "\n") + "\n\n" + block + "\n"
}

func writeIfChanged(path, content string) (bool, error) {
	if current, err := os.ReadFile(path); err == nil && string(current) == content {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, []byte(content), 0o644)
}

func stageLabel(stage string) string {
	switch stage {
	case "phone_screen":
		return "Phone Screen"
	case "interview":
		return "Interview"
	case "onsite":
		return "Onsite"
	case "offer":
		return "Offer"
	default:
		return stage
	}
}

func intOrQuestion(v sql.NullInt64) string {
	if v.Valid {
		return fmt.Sprintf("%d", v.Int64)
	}
	return "?"
}

func datePrefix(v sql.NullString) string {
	if !v.Valid || v.String == "" {
		return ""
	}
	if len(v.String) < 10 {
		return v.String
	}
	return v.String[:10]
}

func floatDaysOrQuestion(v sql.NullFloat64) string {
	if v.Valid {
		return fmt.Sprintf("%.1fd", v.Float64)
	}
	return "?"
}

func avgOrQuestion(v float64, ok bool) string {
	if !ok {
		return "?"
	}
	return fmt.Sprintf("%.1f", v)
}

func average(rows []rejectionRow, value func(rejectionRow) sql.NullFloat64) (float64, bool) {
	var total float64
	var count int
	for _, row := range rows {
		v := value(row)
		if v.Valid {
			total += v.Float64
			count++
		}
	}
	if count == 0 {
		return 0, false
	}
	return total / float64(count), true
}

func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
