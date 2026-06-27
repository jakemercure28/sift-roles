package ats

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"

	"job-search-automation/internal/db"
	"job-search-automation/internal/metrics"
)

const defaultCanonicalizeConcurrency = 5

// CanonicalizeConfig controls the DB-driven canonicalization pass.
type CanonicalizeConfig struct {
	OnlyPending bool
	Concurrency int
	Fetch       Fetcher
	Gemini      GeminiClient
	Log         *slog.Logger
}

// CanonicalizeReportEntry mirrors the resolve-ats-aliases.js report row shape.
type CanonicalizeReportEntry struct {
	ID               string  `json:"id"`
	Title            string  `json:"title"`
	Company          string  `json:"company"`
	Platform         string  `json:"platform"`
	Status           string  `json:"status"`
	Score            int64   `json:"score,omitempty"`
	URL              string  `json:"url"`
	Action           string  `json:"action"`
	ResolvedPlatform string  `json:"resolvedPlatform"`
	ResolvedURL      string  `json:"resolvedUrl"`
	CanonicalID      string  `json:"canonicalId"`
	Confidence       float64 `json:"confidence"`
	Evidence         string  `json:"evidence"`
}

// CanonicalizeReport is the output of one canonicalization pass.
type CanonicalizeReport struct {
	Rows   []CanonicalizeReportEntry `json:"rows"`
	Counts map[string]int            `json:"counts"`
}

// CanonicalizeExisting resolves existing alternate rows and applies the DB
// canonicalization/alias transaction for each row, replacing
// scripts/resolve-ats-aliases.js --apply --only-pending.
func CanonicalizeExisting(ctx context.Context, repo *db.Repository, cfg CanonicalizeConfig) (CanonicalizeReport, error) {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = defaultCanonicalizeConcurrency
	}
	rows, err := repo.SelectAlternateJobs(cfg.OnlyPending)
	if err != nil {
		return CanonicalizeReport{}, err
	}
	if len(rows) == 0 {
		return CanonicalizeReport{Rows: []CanonicalizeReportEntry{}, Counts: map[string]int{}}, nil
	}

	fetch := cfg.Fetch
	if fetch == nil {
		fetch = httpFetcher(nil)
	}

	type resolvedRow struct {
		res *Resolution
		err error
	}
	resolved := make([]resolvedRow, len(rows))
	jobs := make(chan int)
	var wg sync.WaitGroup
	workers := cfg.Concurrency
	if workers > len(rows) {
		workers = len(rows)
	}
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				res, err := ResolveAlternateJob(ctx, atsJobFromDB(rows[i]), ResolveOptions{
					Fetch:  fetch,
					Gemini: cfg.Gemini,
				})
				resolved[i] = resolvedRow{res: res, err: err}
			}
		}()
	}
	for i := range rows {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return CanonicalizeReport{}, ctx.Err()
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()

	report := CanonicalizeReport{
		Rows:   make([]CanonicalizeReportEntry, 0, len(rows)),
		Counts: map[string]int{},
	}
	for i, row := range rows {
		if resolved[i].err != nil {
			return CanonicalizeReport{}, resolved[i].err
		}
		res := resolved[i].res
		if res == nil {
			res = resolution("unresolved", Resolution{Evidence: map[string]any{"reason": "resolver-returned-nil"}})
		}

		dbRes := dbResolutionFromATS(res)
		applied, err := repo.CanonicalizeAlternateJob(row, dbRes)
		if err != nil {
			return CanonicalizeReport{}, err
		}
		entry := reportEntry(row, res, applied)
		report.Rows = append(report.Rows, entry)
		report.Counts[entry.Action]++
		switch entry.Action {
		case "unsupported":
			metrics.ObserveATSResolution("unsupported", entry.Platform, "unsupported")
		case "canonicalized":
			metrics.ObserveATSResolution("canonicalized", entry.Platform, entry.ResolvedPlatform)
		}
		if cfg.Log != nil {
			switch entry.Action {
			case "unsupported":
				cfg.Log.Info("ats resolution unsupported",
					"job_id", entry.ID,
					"company", entry.Company,
					"from_platform", entry.Platform,
				)
			case "canonicalized":
				cfg.Log.Info("ats resolution canonicalized",
					"alternate_job_id", entry.ID,
					"canonical_job_id", entry.CanonicalID,
					"company", entry.Company,
					"from_platform", entry.Platform,
					"to_platform", entry.ResolvedPlatform,
				)
			}
		}
	}
	if cfg.Log != nil && len(report.Rows) > 0 {
		cfg.Log.Info("canonicalized alternate ATS jobs", "counts", report.Counts)
	}
	return report, nil
}

func atsJobFromDB(j *db.Job) Job {
	return Job{
		ID:          j.ID,
		Platform:    nullString(j.Platform),
		Title:       nullString(j.Title),
		Company:     nullString(j.Company),
		URL:         nullString(j.URL),
		Description: nullString(j.Description),
		Location:    nullString(j.Location),
		PostedAt:    nullString(j.PostedAt),
	}
}

func dbResolutionFromATS(res *Resolution) db.Resolution {
	out := db.Resolution{
		Status:     res.Status,
		Platform:   res.Platform,
		URL:        res.URL,
		Confidence: res.Confidence,
		Evidence:   res.Evidence,
	}
	if res.Job != nil {
		out.Job = &db.ResolvedJob{
			ID:          res.Job.ID,
			Title:       res.Job.Title,
			Company:     res.Job.Company,
			URL:         res.Job.URL,
			Platform:    res.Job.Platform,
			Location:    res.Job.Location,
			PostedAt:    res.Job.PostedAt,
			Description: res.Job.Description,
		}
	}
	return out
}

func reportEntry(row *db.Job, res *Resolution, applied db.CanonicalizeResult) CanonicalizeReportEntry {
	canonicalID := applied.CanonicalID
	if canonicalID == "" && res.Job != nil {
		canonicalID = res.Job.ID
	}
	return CanonicalizeReportEntry{
		ID:               row.ID,
		Title:            nullString(row.Title),
		Company:          nullString(row.Company),
		Platform:         nullString(row.Platform),
		Status:           nullString(row.Status),
		Score:            nullInt(row.Score),
		URL:              nullString(row.URL),
		Action:           applied.Action,
		ResolvedPlatform: res.Platform,
		ResolvedURL:      res.URL,
		CanonicalID:      canonicalID,
		Confidence:       res.Confidence,
		Evidence:         formatEvidence(res.Evidence),
	}
}

func formatEvidence(evidence map[string]any) string {
	if evidence == nil {
		return ""
	}
	if v, ok := evidence["method"].(string); ok && v != "" {
		return v
	}
	if v, ok := evidence["unsupportedPlatform"].(string); ok && v != "" {
		return v
	}
	if v, ok := evidence["reason"].(string); ok && v != "" {
		return v
	}
	return ""
}

func nullString(s sql.NullString) string {
	if s.Valid {
		return s.String
	}
	return ""
}

func nullInt(n sql.NullInt64) int64 {
	if n.Valid {
		return n.Int64
	}
	return 0
}
