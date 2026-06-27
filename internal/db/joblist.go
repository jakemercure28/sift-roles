package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// Stats mirrors getGlobalStats in lib/db.js: the dashboard header/sidebar counts.
type Stats struct {
	Total        int `json:"total"`
	NotApplied   int `json:"notApplied"`
	Applied      int `json:"applied"`
	Interviewing int `json:"interviewing"`
	Offers       int `json:"offers"`
	Rejected     int `json:"rejected"`
	Closed       int `json:"closed"`
	Archived     int `json:"archived"`
	Ghosted      int `json:"ghosted"`
}

// GlobalStats buckets all jobs in a single scan, mirroring getGlobalStats.
func (r *Repository) GlobalStats() (Stats, error) {
	var s Stats
	err := r.queryRow(`
		SELECT
			SUM(CASE WHEN status NOT IN ('archived','rejected','closed','ghosted') THEN 1 ELSE 0 END),
			SUM(CASE WHEN status NOT IN ('applied','responded','archived','closed','rejected','ghosted') AND COALESCE(stage, '') NOT IN ('closed','rejected','ghosted') THEN 1 ELSE 0 END),
			SUM(CASE WHEN status IN ('applied','responded') AND stage != 'closed' THEN 1 ELSE 0 END),
			SUM(CASE WHEN stage IN ('phone_screen','interview','onsite','offer') THEN 1 ELSE 0 END),
			SUM(CASE WHEN stage = 'offer' THEN 1 ELSE 0 END),
			SUM(CASE WHEN stage = 'rejected' THEN 1 ELSE 0 END),
			SUM(CASE WHEN stage = 'closed' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status = 'archived' THEN 1 ELSE 0 END),
			SUM(CASE WHEN stage = 'ghosted' THEN 1 ELSE 0 END)
		FROM jobs WHERE user_id = ?`, r.userID).Scan(
		&nullInt{&s.Total}, &nullInt{&s.NotApplied}, &nullInt{&s.Applied},
		&nullInt{&s.Interviewing}, &nullInt{&s.Offers}, &nullInt{&s.Rejected},
		&nullInt{&s.Closed}, &nullInt{&s.Archived}, &nullInt{&s.Ghosted},
	)
	return s, err
}

// StatsRows returns every job with just the fields needed to recompute the
// dashboard counts for a location filter: status/stage for the buckets and
// location/title for metro matching. Lighter than loading full rows, and unlike
// FilteredJobs("all") it includes archived/rejected/closed/ghosted so the counts
// match GlobalStats when no location filter is applied.
func (r *Repository) StatsRows() ([]ListedJob, error) {
	rows, err := r.query(`SELECT location, title, status, stage FROM jobs WHERE user_id = ?`, r.userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ListedJob
	for rows.Next() {
		var j ListedJob
		var location, title, status, stage sql.NullString
		if err := rows.Scan(&location, &title, &status, &stage); err != nil {
			return nil, err
		}
		j.Location, j.Title, j.Status, j.Stage = location.String, title.String, status.String, stage.String
		out = append(out, j)
	}
	return out, rows.Err()
}

// ListedJob is a full jobs row as the dashboard list needs it (render + search).
type ListedJob struct {
	ID                string
	Title             string
	Company           string
	URL               string
	Platform          string
	Location          string
	PostedAt          string
	CreatedAt         string
	UpdatedAt         string
	Description       string
	Score             *int
	Reasoning         string
	RejectionReason   string
	Status            string
	Stage             string
	AppliedAt         string
	RejectedFromStage string
	RejectedAt        string
	ApplyComplexity   string
}

// filterWhere maps a dashboard filter to its WHERE clause (no user input enters
// these literals). closed/ghosted use a fixed updated_at order; the rest take the
// caller's validated orderBy clause. analytics/activity-log return no rows.
var filterWhere = map[string]string{
	"all":          "status NOT IN ('archived','rejected','closed','ghosted')",
	"not-applied":  "status NOT IN ('applied','responded','archived','closed','ghosted') AND COALESCE(stage, '') NOT IN ('closed', 'rejected', 'ghosted')",
	"applied":      "status IN ('applied','responded') AND COALESCE(stage,'applied') NOT IN ('closed','rejected','ghosted')",
	"interviewing": "stage IN ('phone_screen','interview','onsite','offer')",
	"offers":       "stage = 'offer'",
	"rejected":     "stage = 'rejected'",
	"archived":     "status = 'archived'",
}

const listedJobCols = `id, title, company, url, platform, location, posted_at, created_at,
	updated_at, description, score, reasoning, rejection_reasoning, status, stage,
	applied_at, rejected_from_stage, rejected_at, apply_complexity`

// listedJobColsLight is listedJobCols minus the three large free-text columns
// (description, reasoning, rejection_reasoning). The dashboard list view filters,
// sorts and paginates on the small columns and only needs the text for the ~25
// rows actually on screen, so the list query selects this set and hydrates the
// text per-page (JobTextByIDs). Dropping the text columns is the bulk of the bytes
// (descriptions are multi-KB and TOASTed) and is what keeps a thousands-of-rows
// list load from dragging megabytes across the pooler on every cache miss.
const listedJobColsLight = `id, title, company, url, platform, location, posted_at,
	created_at, updated_at, score, status, stage, applied_at, rejected_from_stage,
	rejected_at, apply_complexity`

func filteredJobsWhere(filter string) (string, bool) {
	switch filter {
	case "closed", "ghosted":
		return fmt.Sprintf("stage = '%s'", filter), true
	case "analytics", "activity-log":
		return "", false
	default:
		where, ok := filterWhere[filter]
		if !ok {
			where = filterWhere["all"]
		}
		return where, true
	}
}

// filteredJobsSQL builds the SELECT for a dashboard filter using the given column
// list. ok=false for filters that never list rows (analytics, activity-log).
// closed/ghosted ignore orderBy and use updated_at DESC, matching the JS original.
func filteredJobsSQL(cols, filter, orderBy string) (query string, ok bool) {
	where, ok := filteredJobsWhere(filter)
	if !ok {
		return "", false
	}
	switch filter {
	case "closed", "ghosted":
		return fmt.Sprintf("SELECT %s FROM jobs WHERE (%s) AND user_id = ? ORDER BY updated_at DESC", cols, where), true
	default:
		return fmt.Sprintf("SELECT %s FROM jobs WHERE (%s) AND user_id = ? ORDER BY %s", cols, where, orderBy), true
	}
}

func filteredJobsWhereWithMinScore(filter string, minScore int) (string, bool) {
	where, ok := filteredJobsWhere(filter)
	if !ok {
		return "", false
	}
	if minScore > 1 {
		where = fmt.Sprintf("(%s) AND (score IS NULL OR score >= ?)", where)
	}
	return where, true
}

// FilteredJobs returns the full rows (description/reasoning included) for a
// dashboard filter ordered by orderBy. Used by the search path, which must scan
// the free-text fields; the default list load uses FilteredJobsLight instead.
func (r *Repository) FilteredJobs(filter, orderBy string) ([]ListedJob, error) {
	query, ok := filteredJobsSQL(listedJobCols, filter, orderBy)
	if !ok {
		return nil, nil
	}
	rows, err := r.query(query, r.userID)
	if err != nil {
		return nil, err
	}
	return scanListedJobs(rows)
}

func scanListedJobs(rows Rows) ([]ListedJob, error) {
	defer rows.Close()

	var out []ListedJob
	for rows.Next() {
		var j ListedJob
		var title, company, status sql.NullString
		var url, platform, location, postedAt, createdAt, updatedAt, description sql.NullString
		var reasoning, rejReason, stage, appliedAt, rejFrom, rejAt, complexity sql.NullString
		var score sql.NullInt64
		if err := rows.Scan(
			&j.ID, &title, &company, &url, &platform, &location, &postedAt, &createdAt,
			&updatedAt, &description, &score, &reasoning, &rejReason, &status, &stage,
			&appliedAt, &rejFrom, &rejAt, &complexity,
		); err != nil {
			return nil, err
		}
		j.Title, j.Company, j.Status = title.String, company.String, status.String
		j.URL, j.Platform, j.Location = url.String, platform.String, location.String
		j.PostedAt, j.CreatedAt, j.UpdatedAt = postedAt.String, createdAt.String, updatedAt.String
		j.Description, j.Reasoning, j.RejectionReason = description.String, reasoning.String, rejReason.String
		j.Stage, j.AppliedAt = stage.String, appliedAt.String
		j.RejectedFromStage, j.RejectedAt, j.ApplyComplexity = rejFrom.String, rejAt.String, complexity.String
		if score.Valid {
			v := int(score.Int64)
			j.Score = &v
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// FilteredJobsLight is FilteredJobs without the large free-text columns
// (description/reasoning/rejection_reasoning left empty). Callers that render rows
// must hydrate those fields for the visible page via JobTextByIDs.
func (r *Repository) FilteredJobsLight(filter, orderBy string) ([]ListedJob, error) {
	query, ok := filteredJobsSQL(listedJobColsLight, filter, orderBy)
	if !ok {
		return nil, nil
	}
	rows, err := r.query(query, r.userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ListedJob
	for rows.Next() {
		var j ListedJob
		var title, company, status sql.NullString
		var url, platform, location, postedAt, createdAt, updatedAt sql.NullString
		var stage, appliedAt, rejFrom, rejAt, complexity sql.NullString
		var score sql.NullInt64
		if err := rows.Scan(
			&j.ID, &title, &company, &url, &platform, &location, &postedAt,
			&createdAt, &updatedAt, &score, &status, &stage, &appliedAt, &rejFrom,
			&rejAt, &complexity,
		); err != nil {
			return nil, err
		}
		j.Title, j.Company, j.Status = title.String, company.String, status.String
		j.URL, j.Platform, j.Location = url.String, platform.String, location.String
		j.PostedAt, j.CreatedAt, j.UpdatedAt = postedAt.String, createdAt.String, updatedAt.String
		j.Stage, j.AppliedAt = stage.String, appliedAt.String
		j.RejectedFromStage, j.RejectedAt, j.ApplyComplexity = rejFrom.String, rejAt.String, complexity.String
		if score.Valid {
			v := int(score.Int64)
			j.Score = &v
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// searchableLightCols concatenates the same columns as the dashboard's
// searchableText helper, in the same order, each NULL-coalesced and separated by a
// newline. A search query entered in the UI never contains a newline, so matching
// a query as a substring of this expression is equivalent to strings.Contains over
// the "\n"-joined non-empty fields: the separator stops a match from spanning a
// field boundary, and empty-vs-absent separators cannot create or hide a match for
// a separator-free query.
var searchableLightCols = strings.Join([]string{
	"COALESCE(title, '')", "COALESCE(company, '')", "COALESCE(location, '')",
	"COALESCE(description, '')", "COALESCE(reasoning, '')", "COALESCE(rejection_reasoning, '')",
	"COALESCE(status, '')", "COALESCE(stage, '')", "COALESCE(apply_complexity, '')",
	"COALESCE(platform, '')",
}, " || '\n' || ")

// likeEscaper neutralizes LIKE wildcards so the pattern matches q literally, the
// way strings.Contains does. The escape character is backslash (see the ESCAPE
// clause in FilteredJobsLightSearch).
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// FilteredJobsLightSearch returns the lightweight columns (no large free-text
// fields) for rows in `filter` whose searchable text contains q, applying the same
// min-score floor as the dashboard's in-memory search (NULL scores pass; non-NULL
// scores below minScore are dropped). The text match runs in SQL, so a multi-KB
// description is scanned inside Postgres instead of shipped over the pooler; the
// caller hydrates description/reasoning for just the visible page. Matching mirrors
// applyDashboardSearch/searchableText exactly: case-insensitive literal substring
// over the newline-joined fields. orderBy is a validated clause (resolveOrder), not
// user input; callers re-sort in Go, so it only needs to be valid SQL.
func (r *Repository) FilteredJobsLightSearch(filter, orderBy, q string, minScore int) ([]ListedJob, error) {
	where, ok := filteredJobsWhere(filter)
	if !ok {
		return nil, nil
	}
	// (filter) AND min-score floor AND case-insensitive literal substring match.
	where = "(" + where + ") AND (score IS NULL OR score >= ?)" +
		" AND lower(" + searchableLightCols + ") LIKE '%' || ? || '%' ESCAPE '\\'"
	query := fmt.Sprintf("SELECT %s FROM jobs WHERE %s AND user_id = ? ORDER BY %s",
		listedJobColsLight, where, orderBy)
	pattern := likeEscaper.Replace(strings.ToLower(q))

	rows, err := r.query(query, minScore, pattern, r.userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ListedJob
	for rows.Next() {
		var j ListedJob
		var title, company, status sql.NullString
		var url, platform, location, postedAt, createdAt, updatedAt sql.NullString
		var stage, appliedAt, rejFrom, rejAt, complexity sql.NullString
		var score sql.NullInt64
		if err := rows.Scan(
			&j.ID, &title, &company, &url, &platform, &location, &postedAt,
			&createdAt, &updatedAt, &score, &status, &stage, &appliedAt, &rejFrom,
			&rejAt, &complexity,
		); err != nil {
			return nil, err
		}
		j.Title, j.Company, j.Status = title.String, company.String, status.String
		j.URL, j.Platform, j.Location = url.String, platform.String, location.String
		j.PostedAt, j.CreatedAt, j.UpdatedAt = postedAt.String, createdAt.String, updatedAt.String
		j.Stage, j.AppliedAt = stage.String, appliedAt.String
		j.RejectedFromStage, j.RejectedAt, j.ApplyComplexity = rejFrom.String, rejAt.String, complexity.String
		if score.Valid {
			v := int(score.Int64)
			j.Score = &v
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// FilteredJobsCount returns the number of rows matching filter, optionally
// applying the min-score floor the dashboard uses when no free-text search is
// active. Used by the SQL-paginated list path to compute page bounds without
// fetching every row.
func (r *Repository) FilteredJobsCount(filter string, minScore int) (int, error) {
	where, ok := filteredJobsWhereWithMinScore(filter, minScore)
	if !ok {
		return 0, nil
	}
	query := fmt.Sprintf("SELECT COUNT(*) FROM jobs WHERE (%s) AND user_id = ?", where)
	args := make([]any, 0, 2)
	if minScore > 1 {
		args = append(args, minScore)
	}
	args = append(args, r.userID)
	var n int
	err := r.queryRow(query, args...).Scan(&n)
	return n, err
}

// FilteredJobsLightPage returns a single SQL page of the lightweight list rows,
// applying the same filter and optional min-score floor as FilteredJobsCount.
func (r *Repository) FilteredJobsLightPage(filter, orderBy string, minScore, limit, offset int) ([]ListedJob, error) {
	where, ok := filteredJobsWhereWithMinScore(filter, minScore)
	if !ok {
		return nil, nil
	}
	query := fmt.Sprintf("SELECT %s FROM jobs WHERE (%s) AND user_id = ? ORDER BY %s LIMIT ? OFFSET ?", listedJobColsLight, where, orderBy)
	args := make([]any, 0, 4)
	if minScore > 1 {
		args = append(args, minScore)
	}
	args = append(args, r.userID, limit, offset)

	rows, err := r.query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ListedJob
	for rows.Next() {
		var j ListedJob
		var title, company, status sql.NullString
		var url, platform, location, postedAt, createdAt, updatedAt sql.NullString
		var stage, appliedAt, rejFrom, rejAt, complexity sql.NullString
		var score sql.NullInt64
		if err := rows.Scan(
			&j.ID, &title, &company, &url, &platform, &location, &postedAt,
			&createdAt, &updatedAt, &score, &status, &stage, &appliedAt, &rejFrom,
			&rejAt, &complexity,
		); err != nil {
			return nil, err
		}
		j.Title, j.Company, j.Status = title.String, company.String, status.String
		j.URL, j.Platform, j.Location = url.String, platform.String, location.String
		j.PostedAt, j.CreatedAt, j.UpdatedAt = postedAt.String, createdAt.String, updatedAt.String
		j.Stage, j.AppliedAt = stage.String, appliedAt.String
		j.RejectedFromStage, j.RejectedAt, j.ApplyComplexity = rejFrom.String, rejAt.String, complexity.String
		if score.Valid {
			v := int(score.Int64)
			j.Score = &v
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// JobText holds the large free-text fields omitted from FilteredJobsLight.
type JobText struct {
	Description     string
	Reasoning       string
	RejectionReason string
}

// JobTextByIDs fetches the free-text fields for the given job ids in one query,
// so the list view can hydrate only the rows on the current page. Returns a map
// keyed by job id; ids not found (or other tenants', filtered by RLS/user_id) are
// simply absent. An empty ids slice yields an empty map with no query.
func (r *Repository) JobTextByIDs(ids []string) (map[string]JobText, error) {
	out := make(map[string]JobText, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	query := fmt.Sprintf(
		"SELECT id, description, reasoning, rejection_reasoning FROM jobs WHERE id IN (%s) AND user_id = ?",
		placeholders,
	)
	args := make([]any, 0, len(ids)+1)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, r.userID)

	rows, err := r.query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var description, reasoning, rejReason sql.NullString
		if err := rows.Scan(&id, &description, &reasoning, &rejReason); err != nil {
			return nil, err
		}
		out[id] = JobText{
			Description:     description.String,
			Reasoning:       reasoning.String,
			RejectionReason: rejReason.String,
		}
	}
	return out, rows.Err()
}
