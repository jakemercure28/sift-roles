package dashboard

import "net/http"

// handleMarketActivity serves GET /api/market-activity?period=12w|26w|52w|all,
// the "new roles per week" trend behind the Market Research chart (ported from
// handleMarketActivityApi in lib/dashboard-routes.js). An unrecognized period
// falls back to 26w, matching the Node handler.
func (s *Server) handleMarketActivity(w http.ResponseWriter, r *http.Request) {
	period := r.URL.Query().Get("period")
	switch period {
	case "12w", "26w", "52w", "all":
	default:
		period = "26w"
	}
	rows, err := s.repo.NewRolesByWeek(period)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}
