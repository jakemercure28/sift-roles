package dashboard

import (
	"sort"

	"job-search-automation/internal/db"
)

func stableSortByFloatAsc(rows []db.RejectionInsight, get func(db.RejectionInsight) float64) {
	sort.SliceStable(rows, func(a, b int) bool { return get(rows[a]) < get(rows[b]) })
}

func stableSortByFloatDesc(rows []db.RejectionInsight, get func(db.RejectionInsight) float64) {
	sort.SliceStable(rows, func(a, b int) bool { return get(rows[a]) > get(rows[b]) })
}
