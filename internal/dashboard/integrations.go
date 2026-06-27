package dashboard

import (
	"context"
	"errors"
	"net/http"
	"os"

	"job-search-automation/internal/rejectionsync"
)

// ErrRejectionSyncBusy is returned by the configured sync runner when a sweep
// is already in flight (cron or a previous Sync now click); the run endpoint
// maps it to 409.
var ErrRejectionSyncBusy = errors.New("rejection email sync already running")

// SetRejectionSyncRunner wires the Settings "Sync now" button to a real
// rejection email sync run. Nil (the default) makes the run endpoint report
// "not available".
func (s *Server) SetRejectionSyncRunner(run func(context.Context) (rejectionsync.Summary, error)) {
	s.rejectionSyncRun = run
}

func envTruthy(key string) bool {
	switch os.Getenv(key) {
	case "1", "true", "TRUE", "yes":
		return true
	default:
		return false
	}
}

// GET /api/integrations/rejection-sync -> connection + last-run status for the
// Settings Integrations tab.
func (s *Server) handleRejectionSyncStatus(w http.ResponseWriter, _ *http.Request) {
	payload := map[string]any{
		"configured": os.Getenv("GMAIL_EMAIL") != "" && os.Getenv("GMAIL_APP_PASSWORD") != "",
		"paused":     envTruthy("REJECTION_EMAIL_SYNC_DISABLED"),
		"status":     nil,
	}
	if st, ok, err := s.repo.ReadRejectionSyncStatus(); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	} else if ok {
		payload["status"] = st
	}
	n, err := s.repo.CountRejectionsAppliedSince(7)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	payload["appliedLast7d"] = n
	writeJSON(w, http.StatusOK, payload)
}

// POST /api/integrations/rejection-sync/run -> run one sync synchronously.
// A full sweep is tens of seconds of IMAP traffic; the server sets no write
// timeout and this is a single-user local app, so holding the request open is
// fine. Overlap is rejected with 409 via ErrRejectionSyncBusy.
func (s *Server) handleRejectionSyncRun(w http.ResponseWriter, r *http.Request) {
	if s.rejectionSyncRun == nil {
		jsonError(w, http.StatusServiceUnavailable, "Sync not available in this process")
		return
	}
	sum, err := s.rejectionSyncRun(r.Context())
	if errors.Is(err, ErrRejectionSyncBusy) {
		jsonBusy(w)
		return
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"fetched":   sum.Fetched,
		"applied":   sum.Applied,
		"ignored":   sum.Ignored,
		"unmatched": sum.Unmatched,
	})
}
