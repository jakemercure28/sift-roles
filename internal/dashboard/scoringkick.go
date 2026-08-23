package dashboard

import "time"

// SetScoringKick wires a hook fired (in a goroutine) when a tenant loads the
// dashboard after having no activity inside windowDays: the activity gate
// keep-warms inactive tenants, so their first visit back should trigger an
// immediate scoring pass instead of waiting for the next cron tick. The hook
// must be safe to fire spuriously (the server's in-flight guard and daily
// quotas make a duplicate kick a no-op). windowDays <= 0 or a nil kick
// disables it.
func (s *Server) SetScoringKick(windowDays int, kick func(userID string)) {
	s.scoringKickWindowDays = windowDays
	s.scoringKick = kick
}

// maybeKickScoring fires the scoring kick when the tenant's PREVIOUS last-seen
// stamp is missing or older than the window. Must run before TouchLastSeen
// overwrites the stamp. Best-effort: a failed read just skips the kick (the
// scoring cron remains the sweeper).
func (s *Server) maybeKickScoring() {
	if s.scoringKick == nil || s.scoringKickWindowDays <= 0 {
		return
	}
	prev, err := s.repo.DashboardLastSeen()
	if err != nil {
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -s.scoringKickWindowDays).Format("2006-01-02 15:04:05")
	if prev != "" && prev >= cutoff {
		return
	}
	go s.scoringKick(s.repo.UserID())
}
