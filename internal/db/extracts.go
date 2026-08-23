package db

import (
	"database/sql"
	"strings"
	"time"
)

// This file owns global_job_extracts: the cross-tenant cache of LLM-extracted,
// career-agnostic job facts. Like global_jobs it is deliberately un-tenanted;
// an extract is a market fact about a posting, not tenant state. Fit judgment
// (score/reasoning) stays in the per-tenant jobs table.

// ExtractTarget is a global job that needs (re-)extraction: it backs at least
// one of the calling tenant's pending unscored rows and has no current extract.
type ExtractTarget struct {
	GlobalJobID     string
	Title           string
	Company         string
	Location        string
	Description     string
	DescriptionHash string
}

// GlobalJobExtract is one stored extraction result.
type GlobalJobExtract struct {
	GlobalJobID     string
	DescriptionHash string
	ExtractVersion  int
	Facts           string
	Model           string
}

// PendingExtractTargets returns global jobs that (a) back one of THIS tenant's
// pending unscored rows and (b) lack an extract for the current description
// and version. Scoping by the calling tenant keeps the query inside the RLS
// boundary; cross-tenant dedup falls out of the global table itself, since the
// first tenant to need a job pays for its extract and every later tenant finds
// it already present.
func (r *Repository) PendingExtractTargets(limit, version int) ([]ExtractTarget, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.query(`
		SELECT DISTINCT g.id, g.title, g.company, g.location, g.description, g.description_hash
		FROM jobs j
		JOIN global_jobs g ON g.id = j.global_job_id
		LEFT JOIN global_job_extracts e ON e.global_job_id = g.id
		WHERE j.user_id = ? AND j.score IS NULL AND j.status = 'pending'
		  AND COALESCE(g.description, '') <> ''
		  AND (e.global_job_id IS NULL
		       OR e.description_hash <> g.description_hash
		       OR e.extract_version <> ?)
		LIMIT ?`, r.userID, version, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ExtractTarget
	for rows.Next() {
		var t ExtractTarget
		var title, company, location, description, hash sql.NullString
		if err := rows.Scan(&t.GlobalJobID, &title, &company, &location, &description, &hash); err != nil {
			return nil, err
		}
		t.Title = title.String
		t.Company = company.String
		t.Location = location.String
		t.Description = description.String
		t.DescriptionHash = hash.String
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpsertGlobalJobExtract stores one extraction result, replacing any stale row
// for the same global job (older description hash or older extract version).
func (r *Repository) UpsertGlobalJobExtract(x GlobalJobExtract) error {
	_, err := r.exec(`
		INSERT INTO global_job_extracts
		  (global_job_id, description_hash, extract_version, facts, model, extracted_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(global_job_id) DO UPDATE SET
		  description_hash = excluded.description_hash,
		  extract_version  = excluded.extract_version,
		  facts            = excluded.facts,
		  model            = excluded.model,
		  extracted_at     = excluded.extracted_at`,
		x.GlobalJobID, x.DescriptionHash, x.ExtractVersion, x.Facts, x.Model,
		time.Now().UTC().Format("2006-01-02 15:04:05"))
	return err
}

// CountExtractsToday reports extraction rows written since UTC midnight. The
// extracts table doubles as the extraction budget ledger: it is global (not
// key- or tenant-scoped), so the daily extract cap holds across the whole
// instance no matter which tenant's pass triggered the work.
func (r *Repository) CountExtractsToday() (int, error) {
	cutoff := time.Now().UTC().Truncate(24 * time.Hour).Format("2006-01-02 15:04:05")
	var n int
	err := r.queryRow(`
		SELECT COUNT(*) FROM global_job_extracts WHERE extracted_at >= ?`, cutoff).Scan(&n)
	return n, err
}

// ExtractFactsFor returns the CURRENT extract facts for the tenant's job rows,
// keyed by tenant job id. Stale extracts (description changed since extraction,
// or an older extract version) are omitted, so a caller can safely treat a
// missing key as "score via the full description".
func (r *Repository) ExtractFactsFor(jobIDs []string, version int) (map[string]string, error) {
	if len(jobIDs) == 0 {
		return map[string]string{}, nil
	}
	placeholders := make([]string, len(jobIDs))
	args := make([]any, 0, len(jobIDs)+2)
	args = append(args, r.userID, version)
	for i, id := range jobIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	rows, err := r.query(`
		SELECT j.id, e.facts
		FROM jobs j
		JOIN global_jobs g ON g.id = j.global_job_id
		JOIN global_job_extracts e ON e.global_job_id = j.global_job_id
		WHERE j.user_id = ? AND e.extract_version = ?
		  AND e.description_hash = g.description_hash
		  AND COALESCE(e.facts, '') <> ''
		  AND j.id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string, len(jobIDs))
	for rows.Next() {
		var id string
		var facts sql.NullString
		if err := rows.Scan(&id, &facts); err != nil {
			return nil, err
		}
		if facts.String != "" {
			out[id] = facts.String
		}
	}
	return out, rows.Err()
}
