package db

// Auto-archive reason vocabulary. Every code path that archives a job *without*
// an explicit user action records one of these in jobs.archive_reason, so an
// automatic status mutation is attributable instead of silent. A NULL reason
// means a user archived the row (or it predates the column). Keeping the
// vocabulary in one place is deliberate: the dedup-cascade bug happened because
// several independent passes mutated the same field with no shared accounting.
const (
	ArchiveReasonDedupRepost    = "dedup_repost"
	ArchiveReasonDedupPending   = "dedup_pending"
	ArchiveReasonDedupAlternate = "dedup_alternate"
	ArchiveReasonCanonicalized  = "canonicalized_alternate"
	ArchiveReasonUnsupported    = "canonicalize_unsupported"
	ArchiveReasonLowScore       = "low_score"
	// Prefilter reasons: the pre-score location gate buried the row before any
	// Gemini call. prefilter_non_us is a remote role not open to US candidates;
	// prefilter_location is every other miss against the tenant's saved prefs
	// (not remote under remote-only, excluded unknown location, metro miss).
	ArchiveReasonPrefilterNonUS    = "prefilter_non_us"
	ArchiveReasonPrefilterLocation = "prefilter_location"
	// The listing aged past the stale window before scoring quota ever reached
	// it; a 30+ day old posting is usually filled or ghost, so the Gemini call
	// is better spent on fresh listings.
	ArchiveReasonStaleUnscored = "stale_unscored"
)

// CountCascadedArchives reports how many (title, company) groups for the tenant
// were left with no live, scoreable row by an automatic dedup/canonicalize
// archive: every row in the group is archived, every row is unscored, and at
// least one was archived by a dedup or canonicalize pass. That is exactly the
// invariant the dedup-cascade bug violated (it deleted unique listings to zero
// rows), so a non-zero count is a regression canary, not normal operation.
//
// User-dismissed unscored jobs are excluded because their archive_reason is
// NULL, so this does not fire on legitimate manual archiving.
func (r *Repository) CountCascadedArchives() (int, error) {
	var n int
	err := r.queryRow(`
		SELECT COUNT(*) FROM (
			SELECT 1
			FROM jobs
			WHERE user_id = ?
			GROUP BY LOWER(TRIM(title)), LOWER(TRIM(company))
			HAVING MIN(CASE WHEN status = 'archived' THEN 1 ELSE 0 END) = 1
			   AND MIN(CASE WHEN score IS NULL THEN 1 ELSE 0 END) = 1
			   AND MAX(CASE WHEN archive_reason IN (
			       '`+ArchiveReasonDedupRepost+`',
			       '`+ArchiveReasonDedupPending+`',
			       '`+ArchiveReasonDedupAlternate+`',
			       '`+ArchiveReasonCanonicalized+`'
			   ) THEN 1 ELSE 0 END) = 1
		) g`, r.userID).Scan(&n)
	return n, err
}
