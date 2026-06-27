package dashboard

import (
	"net/http"
	"os"
	"strings"

	"job-search-automation/internal/middleware"
)

// handleDeleteAccount permanently deletes the signed-in tenant: every row across
// the user_id-scoped tables (via repo.DeleteTenant) plus the on-disk profile
// directory (resume.md, context.md, companies.json, ...). The browser signs the
// user out afterwards.
//
// Hosted-only: the Delete account button is injected with the auth chrome, and
// the LocalUser/empty-uid guard refuses to run on self-host so the shared root
// data dir can never be wiped. s is the per-request tenant-scoped clone, so
// s.repo is already ForUser(uid) and s.dataDir is that tenant's profile dir.
func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r.Context())
	if uid == "" || uid == middleware.LocalUser {
		jsonError(w, http.StatusForbidden, "account deletion is only available for hosted accounts")
		return
	}
	if _, err := s.repo.DeleteTenant(); err != nil {
		s.log.Error("account deletion failed", "error", err)
		jsonError(w, http.StatusInternalServerError, "could not delete account")
		return
	}
	// Remove the tenant's profile directory. Only when it is clearly this tenant's
	// own dir (non-empty and path-scoped to the uid), so a misconfigured shared
	// root is never deleted even if forUser were bypassed.
	if dir := s.dataDir; dir != "" && strings.Contains(dir, uid) {
		if err := os.RemoveAll(dir); err != nil {
			// Rows are already gone; a leftover dir is not worth failing the request.
			s.log.Warn("account profile dir cleanup failed", "dir", dir, "error", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
