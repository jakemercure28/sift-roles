package db

import (
	"context"
	"database/sql"
	"strings"

	"job-search-automation/internal/discovery"
)

// registryFreshDays bounds how long a verified board is trusted without a fresh
// HTTP probe. Past this, discovery re-verifies and the row's last_verified_at is
// bumped, so a board that has gone dead eventually drops out of the trusted set.
const registryFreshDays = 14

// CompanyRegistry exposes this repository's global company_registry table as a
// discovery.Registry. The table is GLOBAL: unlike every other table it has no
// user_id column and no RLS policy, so reads and writes here intentionally ignore
// the repository's tenant scope. The tenant id is recorded as first_seen_by for
// attribution only.
func (r *Repository) CompanyRegistry() discovery.Registry {
	return companyRegistry{repo: r}
}

type companyRegistry struct{ repo *Repository }

// Known returns boards verified within registryFreshDays, shaped as a
// discovery.Suggested so discovery can index them for cache lookups.
func (c companyRegistry) Known(ctx context.Context) (discovery.Suggested, error) {
	out := discovery.Suggested{
		Greenhouse: []string{},
		Ashby:      []string{},
		Lever:      []string{},
		Workday:    []discovery.WorkdayEntry{},
	}
	rows, err := c.repo.query(
		`SELECT platform, slug, wd, board, label FROM company_registry
		 WHERE last_verified_at >= datetime('now', '-' || ? || ' days')`,
		registryFreshDays)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var platform, slug, board, label sql.NullString
		var wd sql.NullInt64
		if err := rows.Scan(&platform, &slug, &wd, &board, &label); err != nil {
			return out, err
		}
		switch strings.ToLower(platform.String) {
		case "greenhouse":
			if slug.String != "" {
				out.Greenhouse = append(out.Greenhouse, slug.String)
			}
		case "ashby":
			if slug.String != "" {
				out.Ashby = append(out.Ashby, slug.String)
			}
		case "lever":
			if slug.String != "" {
				out.Lever = append(out.Lever, slug.String)
			}
		case "workday":
			out.Workday = append(out.Workday, discovery.WorkdayEntry{
				Sub:   slug.String,
				WD:    int(wd.Int64),
				Board: board.String,
				Label: label.String,
			})
		}
	}
	return out, rows.Err()
}

// Upsert records freshly verified boards, bumping last_verified_at on rows that
// already exist so they stay in the trusted set.
func (c companyRegistry) Upsert(ctx context.Context, boards []discovery.VerifiedBoard) error {
	for _, b := range boards {
		platform := strings.ToLower(strings.TrimSpace(b.Platform))
		var key, slug, board, label string
		var wd int
		if platform == "workday" {
			if b.Workday == nil {
				continue
			}
			key = discovery.WorkdayKey(*b.Workday)
			if key == "" {
				continue
			}
			slug = b.Workday.Sub
			wd = b.Workday.WD
			board = b.Workday.Board
			label = b.Workday.Label
		} else {
			if b.Slug == "" {
				continue
			}
			key = discovery.SlugKey(b.Slug)
			slug = b.Slug
		}
		if _, err := c.repo.exec(
			`INSERT INTO company_registry
			   (platform, registry_key, slug, label, wd, board, first_seen_by, verified_at, last_verified_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))
			 ON CONFLICT(platform, registry_key) DO UPDATE SET last_verified_at = datetime('now')`,
			platform, key, slug, label, wd, board, c.repo.userID,
		); err != nil {
			return err
		}
	}
	return nil
}
