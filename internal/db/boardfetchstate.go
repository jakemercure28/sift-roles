package db

import (
	"database/sql"
	"strings"
)

// Incremental board fetching state, stored on company_registry rows (see
// migration registry_fetch_state). The global board pass loads the previous
// cycle's etag/content-hash per board, hands it to the worker, and persists
// the outcomes; a board with no registry row simply has no state and is
// always fully fetched.

// BoardKey identifies one board's registry row.
type BoardKey struct {
	Platform    string // lowercase platform key (greenhouse|lever|ashby|...)
	RegistryKey string // registry_key (discovery.SlugKey / WorkdayKey)
}

// BoardFetchState is the persisted incremental-fetch state for one board.
type BoardFetchState struct {
	Etag        string
	ContentHash string
}

// LoadBoardFetchStates returns the stored fetch state for the given boards.
// Boards without a registry row (or with no recorded state) are absent from
// the result.
func (r *Repository) LoadBoardFetchStates(keys []BoardKey) (map[BoardKey]BoardFetchState, error) {
	out := map[BoardKey]BoardFetchState{}
	if len(keys) == 0 {
		return out, nil
	}
	// Boards arrive grouped by platform in practice; a simple OR list keeps the
	// query dialect-neutral and the key count is bounded by the chunk size.
	clauses := make([]string, len(keys))
	args := make([]any, 0, 2*len(keys))
	for i, k := range keys {
		clauses[i] = "(platform = ? AND registry_key = ?)"
		args = append(args, k.Platform, k.RegistryKey)
	}
	rows, err := r.query(`
		SELECT platform, registry_key, etag, last_content_hash
		FROM company_registry
		WHERE `+strings.Join(clauses, " OR "), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var k BoardKey
		var etag, hash sql.NullString
		if err := rows.Scan(&k.Platform, &k.RegistryKey, &etag, &hash); err != nil {
			return nil, err
		}
		if etag.String == "" && hash.String == "" {
			continue
		}
		out[k] = BoardFetchState{Etag: etag.String, ContentHash: hash.String}
	}
	return out, rows.Err()
}

// SaveBoardFetchResult persists one board's fetch outcome. Only existing
// registry rows are updated: the fetch pass must not grow the registry with
// unverified boards, and a board outside the registry just keeps full-fetching.
func (r *Repository) SaveBoardFetchResult(key BoardKey, status, etag, contentHash string) error {
	_, err := r.exec(`
		UPDATE company_registry
		SET last_fetch_status = ?, etag = ?, last_content_hash = ?, last_fetched_at = datetime('now')
		WHERE platform = ? AND registry_key = ?`,
		status, etag, contentHash, key.Platform, key.RegistryKey)
	return err
}
