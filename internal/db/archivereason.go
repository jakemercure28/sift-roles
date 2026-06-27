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
